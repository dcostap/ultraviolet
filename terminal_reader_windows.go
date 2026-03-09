//go:build windows
// +build windows

package uv

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/charmbracelet/x/ansi"
	xwindows "github.com/charmbracelet/x/windows"
	"github.com/muesli/cancelreader"
	"golang.org/x/sys/windows"
)

const (
	win32BracketedPasteStart     = "\x1b[200~"
	win32BracketedPasteEnd       = "\x1b[201~"
	win32PasteCoalesceWindow     = 5 * time.Millisecond
	win32PasteRuneThreshold      = 24
	win32PasteMultilineThreshold = 8
)

// streamData sends data from the input stream to the event channel.
func (d *TerminalReader) streamData(ctx context.Context, readc chan []byte) error {
	cc, ok := d.r.(*conInputReader)
	if !ok {
		d.logf("streamData: reader is not a conInputReader, falling back to default implementation")
		return d.sendBytes(ctx, readc)
	}

	// Store the value of VT Input Mode for later use.
	d.vtInput = cc.newMode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT != 0

	var buf bytes.Buffer
	var records []xwindows.InputRecord
	var err error
	for {
		for {
			records, err = peekNConsoleInputs(cc.conin, readBufSize)
			if cc.isCanceled() {
				return cancelreader.ErrCanceled
			}
			if err != nil {
				return err
			}
			if len(records) > 0 {
				break
			}

			if d.hasPendingWin32VTText() && time.Now().After(d.win32VTTextDeadline) {
				d.flushPendingWin32VTText(&buf)
				if err := sendWin32SerializedInput(ctx, readc, &buf); err != nil {
					return err
				}
				continue
			}

			// Sleep for a bit to avoid busy waiting.
			time.Sleep(d.win32InputPollDelay())
		}

		records, err = readNConsoleInputs(cc.conin, uint32(len(records))) //nolint:gosec
		if cc.isCanceled() {
			return cancelreader.ErrCanceled
		}
		if err != nil {
			return err
		}

		// We convert Windows Input Records to VT input sequences for easier
		// processing especially when dealing with UTF-16 decoding and
		// Win32-Input-Mode processing.
		d.serializeWin32InputRecords(records, &buf)

		if err := sendWin32SerializedInput(ctx, readc, &buf); err != nil {
			return err
		}
	}
}

// serializeWin32InputRecords serializes the Win32 input events converting them
// to valid VT input sequences. It will also encode any UTF-16 pairs that might
// be present in the input buffer. The resulting byte slice can be sent to the
// terminal as input.
func (d *TerminalReader) serializeWin32InputRecords(records []xwindows.InputRecord, buf *bytes.Buffer) {
	for _, record := range records {
		if d.vtInput && record.EventType != xwindows.KEY_EVENT {
			d.flushPendingWin32VTText(buf)
		}

		switch record.EventType {
		case xwindows.KEY_EVENT:
			kevent := record.KeyEvent()
			// d.logf("key event: %s", keyEventString(kevent.VirtualKeyCode, kevent.VirtualScanCode, kevent.Char, kevent.KeyDown, kevent.ControlKeyState, kevent.RepeatCount))

			if d.vtInput {
				if d.shouldIgnoreWin32VTTextEvent(kevent) {
					continue
				}
				if d.isWin32VTTextEvent(kevent) {
					d.appendPendingWin32VTTextEvent(kevent)
					continue
				}
				d.flushPendingWin32VTText(buf)
			} else {
				var kd int
				if kevent.KeyDown {
					kd = 1
				}
				// We encode the key to Win32 Input Mode if it is a known key.
				if kevent.VirtualKeyCode == 0 {
					d.eventScanner.storeGraphemeRune(kd, kevent.Char)
				} else {
					buf.Write(d.eventScanner.encodeGraphemeBufs())
					fmt.Fprintf(buf,
						"\x1b[%d;%d;%d;%d;%d;%d_",
						kevent.VirtualKeyCode,
						kevent.VirtualScanCode,
						kevent.Char,
						kd,
						kevent.ControlKeyState,
						kevent.RepeatCount)
				}
			}

		case xwindows.MOUSE_EVENT:
			if d.MouseMode == nil || *d.MouseMode == 0 {
				continue
			}
			mouseMode := *d.MouseMode
			mevent := record.MouseEvent()

			var isRelease bool
			var isMotion bool
			var button MouseButton
			alt := mevent.ControlKeyState&(xwindows.LEFT_ALT_PRESSED|xwindows.RIGHT_ALT_PRESSED) != 0
			ctrl := mevent.ControlKeyState&(xwindows.LEFT_CTRL_PRESSED|xwindows.RIGHT_CTRL_PRESSED) != 0
			shift := mevent.ControlKeyState&(xwindows.SHIFT_PRESSED) != 0
			wheelDirection := int16(highWord(mevent.ButtonState)) //nolint:gosec
			switch mevent.EventFlags {
			case 0, xwindows.DOUBLE_CLICK:
				button, isRelease = mouseEventButton(d.lastMouseBtns, mevent.ButtonState)
			case xwindows.MOUSE_WHEELED:
				if wheelDirection > 0 {
					button = MouseWheelUp
				} else {
					button = MouseWheelDown
				}
			case xwindows.MOUSE_HWHEELED:
				if wheelDirection > 0 {
					button = MouseWheelRight
				} else {
					button = MouseWheelLeft
				}
			case xwindows.MOUSE_MOVED:
				button, _ = mouseEventButton(d.lastMouseBtns, mevent.ButtonState)
				isMotion = true
			}

			// We emulate mouse mode levels on Windows. This is because Windows
			// doesn't have a concept of different mouse modes. We use the mouse mode to determine
			if button == MouseNone && mouseMode&MouseModeMotion == 0 ||
				(button != MouseNone && mouseMode&MouseModeDrag == 0) {
				continue
			}

			// Encode mouse events as SGR mouse sequences that can be read by [EventDecoder].
			buf.WriteString(ansi.MouseSgr(
				ansi.EncodeMouseButton(button, isMotion, shift, alt, ctrl),
				int(mevent.MousePositon.X), int(mevent.MousePositon.Y), isRelease,
			))

			d.lastMouseBtns = mevent.ButtonState

		case xwindows.WINDOW_BUFFER_SIZE_EVENT:
			wevent := record.WindowBufferSizeEvent()
			if wevent.Size.X != d.lastWinsizeX || wevent.Size.Y != d.lastWinsizeY {
				d.lastWinsizeX, d.lastWinsizeY = wevent.Size.X, wevent.Size.Y
				// We encode window resize events as CSI 4 ; height ; width t
				// sequence which the [EventDecoder] understands.
				buf.WriteString(
					ansi.WindowOp(
						8,                  // Terminal window size in cells
						int(wevent.Size.Y), // height
						int(wevent.Size.X), // width
					),
				)
			}

		case xwindows.FOCUS_EVENT:
			fevent := record.FocusEvent()
			if fevent.SetFocus {
				buf.WriteString(ansi.Focus)
			} else {
				buf.WriteString(ansi.Blur)
			}

		case xwindows.MENU_EVENT:
			// ignore
		}
	}

	// Flush any remaining grapheme buffers.
	buf.Write(d.eventScanner.encodeGraphemeBufs())
}

func shouldCoalesceWin32Paste(runeCount int, hasNewline bool) bool {
	if runeCount >= win32PasteRuneThreshold {
		return true
	}
	return hasNewline && runeCount >= win32PasteMultilineThreshold
}

func (d *TerminalReader) appendPendingWin32VTTextEvent(kevent xwindows.KeyEventRecord) {
	d.win32VTTextActive = true
	d.win32VTTextDeadline = time.Now().Add(win32PasteCoalesceWindow)

	if d.appendWin32VTTextEvent(kevent, &d.win32VTText) {
		d.win32VTTextRunes++
		d.win32VTTextHasNewline = d.win32VTTextHasNewline || kevent.Char == '\r' || kevent.Char == '\n'
	}
}

func (d *TerminalReader) appendWin32VTTextEvent(kevent xwindows.KeyEventRecord, buf *strings.Builder) bool {
	if d.utf16Half[1] {
		d.utf16Half[1] = false
		d.utf16Buf[1][1] = kevent.Char
		buf.WriteRune(utf16.DecodeRune(d.utf16Buf[1][0], d.utf16Buf[1][1]))
		return true
	}
	if utf16.IsSurrogate(kevent.Char) {
		d.utf16Half[1] = true
		d.utf16Buf[1][0] = kevent.Char
		return false
	}
	if !kevent.KeyDown {
		return false
	}
	buf.WriteRune(kevent.Char)
	return true
}

func (d *TerminalReader) flushPendingWin32VTText(buf *bytes.Buffer) {
	if !d.win32VTTextActive {
		return
	}
	if d.utf16Half[1] && d.win32VTText.Len() == 0 {
		return
	}

	text := d.win32VTText.String()
	if shouldCoalesceWin32Paste(d.win32VTTextRunes, d.win32VTTextHasNewline) {
		buf.WriteString(win32BracketedPasteStart)
		buf.WriteString(text)
		buf.WriteString(win32BracketedPasteEnd)
	} else {
		buf.WriteString(text)
	}

	d.win32VTText.Reset()
	d.win32VTTextActive = false
	d.win32VTTextRunes = 0
	d.win32VTTextHasNewline = false
}

func (d *TerminalReader) hasPendingWin32VTText() bool {
	return d.win32VTTextActive
}

func (d *TerminalReader) win32InputPollDelay() time.Duration {
	if !d.hasPendingWin32VTText() {
		return 10 * time.Millisecond
	}

	remaining := time.Until(d.win32VTTextDeadline)
	if remaining <= 0 {
		return 0
	}
	if remaining > time.Millisecond {
		return time.Millisecond
	}
	return remaining
}

func sendWin32SerializedInput(ctx context.Context, readc chan []byte, buf *bytes.Buffer) error {
	if buf.Len() == 0 {
		return nil
	}

	select {
	case <-ctx.Done():
		return nil
	case readc <- buf.Bytes():
	}

	buf.Reset()
	return nil
}

func (d *TerminalReader) isWin32VTTextEvent(kevent xwindows.KeyEventRecord) bool {
	if d.utf16Half[1] {
		return true
	}
	if !kevent.KeyDown {
		return false
	}
	return kevent.Char != 0
}

func (d *TerminalReader) shouldIgnoreWin32VTTextEvent(kevent xwindows.KeyEventRecord) bool {
	if !kevent.KeyDown {
		return true
	}
	if kevent.Char != 0 {
		return false
	}
	switch kevent.VirtualKeyCode {
	case windows.VK_SHIFT, windows.VK_CONTROL, windows.VK_MENU,
		windows.VK_LSHIFT, windows.VK_RSHIFT,
		windows.VK_LCONTROL, windows.VK_RCONTROL,
		windows.VK_LMENU, windows.VK_RMENU:
		return true
	default:
		return false
	}
}

func mouseEventButton(p, s uint32) (MouseButton, bool) {
	var isRelease bool
	button := MouseNone
	btn := p ^ s
	if btn&s == 0 {
		isRelease = true
	}

	if btn == 0 {
		switch {
		case s&xwindows.FROM_LEFT_1ST_BUTTON_PRESSED > 0:
			button = MouseLeft
		case s&xwindows.FROM_LEFT_2ND_BUTTON_PRESSED > 0:
			button = MouseMiddle
		case s&xwindows.RIGHTMOST_BUTTON_PRESSED > 0:
			button = MouseRight
		case s&xwindows.FROM_LEFT_3RD_BUTTON_PRESSED > 0:
			button = MouseBackward
		case s&xwindows.FROM_LEFT_4TH_BUTTON_PRESSED > 0:
			button = MouseForward
		}
		return button, isRelease
	}

	switch btn {
	case xwindows.FROM_LEFT_1ST_BUTTON_PRESSED: // left button
		button = MouseLeft
	case xwindows.RIGHTMOST_BUTTON_PRESSED: // right button
		button = MouseRight
	case xwindows.FROM_LEFT_2ND_BUTTON_PRESSED: // middle button
		button = MouseMiddle
	case xwindows.FROM_LEFT_3RD_BUTTON_PRESSED: // unknown (possibly mouse backward)
		button = MouseBackward
	case xwindows.FROM_LEFT_4TH_BUTTON_PRESSED: // unknown (possibly mouse forward)
		button = MouseForward
	}

	return button, isRelease
}

func highWord(data uint32) uint16 {
	return uint16((data & 0xFFFF0000) >> 16) //nolint:gosec
}

func readNConsoleInputs(console windows.Handle, maxEvents uint32) ([]xwindows.InputRecord, error) {
	if maxEvents == 0 {
		return nil, fmt.Errorf("maxEvents cannot be zero")
	}

	records := make([]xwindows.InputRecord, maxEvents)
	n, err := readConsoleInput(console, records)
	return records[:n], err
}

func readConsoleInput(console windows.Handle, inputRecords []xwindows.InputRecord) (uint32, error) {
	if len(inputRecords) == 0 {
		return 0, fmt.Errorf("size of input record buffer cannot be zero")
	}

	var read uint32

	err := xwindows.ReadConsoleInput(console, &inputRecords[0], uint32(len(inputRecords)), &read) //nolint:gosec

	return read, err //nolint:wrapcheck
}

func peekConsoleInput(console windows.Handle, inputRecords []xwindows.InputRecord) (uint32, error) {
	if len(inputRecords) == 0 {
		return 0, fmt.Errorf("size of input record buffer cannot be zero")
	}

	var read uint32

	err := xwindows.PeekConsoleInput(console, &inputRecords[0], uint32(len(inputRecords)), &read) //nolint:gosec

	return read, err //nolint:wrapcheck
}

func peekNConsoleInputs(console windows.Handle, maxEvents uint32) ([]xwindows.InputRecord, error) {
	if maxEvents == 0 {
		return nil, fmt.Errorf("maxEvents cannot be zero")
	}

	records := make([]xwindows.InputRecord, maxEvents)
	n, err := peekConsoleInput(console, records)
	return records[:n], err
}

//nolint:unused
func keyEventString(vkc, sc uint16, r rune, keyDown bool, cks uint32, repeatCount uint16) string {
	var s strings.Builder
	s.WriteString("vkc: ")
	s.WriteString(fmt.Sprintf("%d, 0x%02x", vkc, vkc))
	s.WriteString(", sc: ")
	s.WriteString(fmt.Sprintf("%d, 0x%02x", sc, sc))
	s.WriteString(", r: ")
	s.WriteString(fmt.Sprintf("%q 0x%x", r, r))
	s.WriteString(", down: ")
	s.WriteString(fmt.Sprintf("%v", keyDown))
	s.WriteString(", cks: [")
	if cks&xwindows.LEFT_ALT_PRESSED != 0 {
		s.WriteString("left alt, ")
	}
	if cks&xwindows.RIGHT_ALT_PRESSED != 0 {
		s.WriteString("right alt, ")
	}
	if cks&xwindows.LEFT_CTRL_PRESSED != 0 {
		s.WriteString("left ctrl, ")
	}
	if cks&xwindows.RIGHT_CTRL_PRESSED != 0 {
		s.WriteString("right ctrl, ")
	}
	if cks&xwindows.SHIFT_PRESSED != 0 {
		s.WriteString("shift, ")
	}
	if cks&xwindows.CAPSLOCK_ON != 0 {
		s.WriteString("caps lock, ")
	}
	if cks&xwindows.NUMLOCK_ON != 0 {
		s.WriteString("num lock, ")
	}
	if cks&xwindows.SCROLLLOCK_ON != 0 {
		s.WriteString("scroll lock, ")
	}
	if cks&xwindows.ENHANCED_KEY != 0 {
		s.WriteString("enhanced key, ")
	}
	s.WriteString("], repeat count: ")
	s.WriteString(fmt.Sprintf("%d", repeatCount))
	return s.String()
}
