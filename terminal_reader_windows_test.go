//go:build windows
// +build windows

package uv

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	xwindows "github.com/charmbracelet/x/windows"
	"golang.org/x/sys/windows"
)

func TestSerializeWin32InputRecordsCoalescesMultilinePaste(t *testing.T) {
	drv := NewTerminalReader(bytes.NewReader(nil), "xterm-256color")
	drv.vtInput = true

	records := append(win32TextRecords("first line"), win32ModifierRecord(windows.VK_SHIFT, true))
	records = append(records, win32ModifierRecord(windows.VK_SHIFT, false))
	records = append(records, win32TextRecords("\rsecond line")...)

	var buf bytes.Buffer
	drv.serializeWin32InputRecords(records, &buf)
	drv.flushPendingWin32VTText(&buf)

	_, events := drv.eventScanner.scanEvents(buf.Bytes(), true)
	want := []Event{
		PasteStartEvent{},
		PasteEvent{"first line\rsecond line"},
		PasteEndEvent{},
	}

	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected events:\nwant: %#v\ngot:  %#v", want, events)
	}
}

func TestSerializeWin32InputRecordsLeavesSmallTextAsKeyPresses(t *testing.T) {
	drv := NewTerminalReader(bytes.NewReader(nil), "xterm-256color")
	drv.vtInput = true

	var buf bytes.Buffer
	for _, record := range win32TextRecords("hello") {
		drv.serializeWin32InputRecords([]xwindows.InputRecord{record}, &buf)
		drv.flushPendingWin32VTText(&buf)
	}

	_, events := drv.eventScanner.scanEvents(buf.Bytes(), true)
	want := []Event{
		KeyPressEvent{Code: 'h', Text: "h"},
		KeyPressEvent{Code: 'e', Text: "e"},
		KeyPressEvent{Code: 'l', Text: "l"},
		KeyPressEvent{Code: 'l', Text: "l"},
		KeyPressEvent{Code: 'o', Text: "o"},
	}

	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected events:\nwant: %#v\ngot:  %#v", want, events)
	}
}

func TestSerializeWin32InputRecordsCoalescesShortRapidBurst(t *testing.T) {
	drv := NewTerminalReader(bytes.NewReader(nil), "xterm-256color")
	drv.vtInput = true

	var buf bytes.Buffer
	drv.serializeWin32InputRecords(win32TextRecords("hello"), &buf)
	drv.flushPendingWin32VTText(&buf)

	_, events := drv.eventScanner.scanEvents(buf.Bytes(), true)
	want := []Event{
		PasteStartEvent{},
		PasteEvent{"hello"},
		PasteEndEvent{},
	}

	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected events:\nwant: %#v\ngot:  %#v", want, events)
	}
}

func TestSerializeWin32InputRecordsPreservesAltGrText(t *testing.T) {
	drv := NewTerminalReader(bytes.NewReader(nil), "xterm-256color")
	drv.vtInput = true

	var buf bytes.Buffer
	drv.serializeWin32InputRecords([]xwindows.InputRecord{
		win32KeyRecord('€', true, 0, xwindows.LEFT_CTRL_PRESSED|xwindows.RIGHT_ALT_PRESSED),
	}, &buf)
	drv.flushPendingWin32VTText(&buf)

	_, events := drv.eventScanner.scanEvents(buf.Bytes(), true)
	want := []Event{
		KeyPressEvent{Code: '€', Text: "€"},
	}

	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected events:\nwant: %#v\ngot:  %#v", want, events)
	}
}

func TestSerializeWin32InputRecordsCoalescesAcrossCalls(t *testing.T) {
	drv := NewTerminalReader(bytes.NewReader(nil), "xterm-256color")
	drv.vtInput = true

	var buf bytes.Buffer
	drv.serializeWin32InputRecords(win32TextRecords("first line\rsecond"), &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected cross-call text to stay pending, got %q", buf.String())
	}

	drv.serializeWin32InputRecords(win32TextRecords(" line"), &buf)
	drv.flushPendingWin32VTText(&buf)

	_, events := drv.eventScanner.scanEvents(buf.Bytes(), true)
	want := []Event{
		PasteStartEvent{},
		PasteEvent{"first line\rsecond line"},
		PasteEndEvent{},
	}

	if !reflect.DeepEqual(events, want) {
		t.Fatalf("unexpected events:\nwant: %#v\ngot:  %#v", want, events)
	}
}

func win32TextRecords(text string) []xwindows.InputRecord {
	records := make([]xwindows.InputRecord, 0, len(text))
	for _, r := range text {
		records = append(records, win32KeyRecord(r, true, 0, 0))
	}
	return records
}

func win32ModifierRecord(vkey uint16, keyDown bool) xwindows.InputRecord {
	return win32KeyRecord(0, keyDown, vkey, 0)
}

func win32KeyRecord(char rune, keyDown bool, vkey uint16, controlState uint32) xwindows.InputRecord {
	var record xwindows.InputRecord
	record.EventType = xwindows.KEY_EVENT

	if keyDown {
		binary.LittleEndian.PutUint32(record.Event[0:4], 1)
	}
	binary.LittleEndian.PutUint16(record.Event[4:6], 1)
	binary.LittleEndian.PutUint16(record.Event[6:8], vkey)
	binary.LittleEndian.PutUint16(record.Event[10:12], uint16(char))
	binary.LittleEndian.PutUint32(record.Event[12:16], controlState)

	return record
}
