//go:build windows

package main

import (
	"errors"
	"io"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	archiveTrayCallbackMessage = 0x0401
	wmDestroy                  = 0x0002
	wmLeftButtonUp             = 0x0202
	nifMessage                 = 0x00000001
	nifIcon                    = 0x00000002
	nifTip                     = 0x00000004
	nimAdd                     = 0x00000000
	nimDelete                  = 0x00000002
	idiApplication             = 32512
	swHide                     = 0
)

type windowsPoint struct {
	X int32
	Y int32
}

type windowsMessage struct {
	Window  windows.Handle
	Message uint32
	_       uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   windowsPoint
	Private uint32
}

type windowsClass struct {
	Size            uint32
	Style           uint32
	WindowProcedure uintptr
	ClassExtra      int32
	WindowExtra     int32
	Instance        windows.Handle
	Icon            windows.Handle
	Cursor          windows.Handle
	Background      windows.Handle
	MenuName        *uint16
	ClassName       *uint16
	SmallIcon       windows.Handle
}

type windowsNotifyIcon struct {
	Size             uint32
	Window           windows.Handle
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	VersionOrTimeout uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUID             windows.GUID
	BalloonIcon      windows.Handle
}

type windowsTrayState struct {
	notify         windowsNotifyIcon
	taskbarCreated uint32
	openDashboard  func() error
}

var (
	user32                     = windows.NewLazySystemDLL("user32.dll")
	shell32                    = windows.NewLazySystemDLL("shell32.dll")
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	registerClassExW           = user32.NewProc("RegisterClassExW")
	createWindowExW            = user32.NewProc("CreateWindowExW")
	defWindowProcW             = user32.NewProc("DefWindowProcW")
	getMessageW                = user32.NewProc("GetMessageW")
	translateMessage           = user32.NewProc("TranslateMessage")
	dispatchMessageW           = user32.NewProc("DispatchMessageW")
	postQuitMessage            = user32.NewProc("PostQuitMessage")
	loadIconW                  = user32.NewProc("LoadIconW")
	registerWindowMessageW     = user32.NewProc("RegisterWindowMessageW")
	showWindow                 = user32.NewProc("ShowWindow")
	shellNotifyIconW           = shell32.NewProc("Shell_NotifyIconW")
	getModuleHandleW           = kernel32.NewProc("GetModuleHandleW")
	getConsoleWindow           = kernel32.NewProc("GetConsoleWindow")
	activeWindowsTray          *windowsTrayState
	windowsTrayWindowProcedure = syscall.NewCallback(handleWindowsTrayMessage)
)

func startChromeProcess(executable string, arguments ...string) error {
	command := exec.Command(executable, arguments...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Start()
}

func runPlatformTray(_ trayConfiguration, openDashboard func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if console, _, _ := getConsoleWindow.Call(); console != 0 {
		showWindow.Call(console, swHide)
	}
	instance, _, callErr := getModuleHandleW.Call(0)
	if instance == 0 {
		return callErr
	}
	className, err := windows.UTF16PtrFromString("ArchiveTrayWindow")
	if err != nil {
		return err
	}
	windowClass := windowsClass{
		Size:            uint32(unsafe.Sizeof(windowsClass{})),
		WindowProcedure: windowsTrayWindowProcedure,
		Instance:        windows.Handle(instance),
		ClassName:       className,
	}
	atom, _, registerErr := registerClassExW.Call(uintptr(unsafe.Pointer(&windowClass)))
	if atom == 0 {
		return registerErr
	}
	window, _, createErr := createWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0,
		instance,
		0,
	)
	if window == 0 {
		return createErr
	}
	icon, _, iconErr := loadIconW.Call(0, idiApplication)
	if icon == 0 {
		return iconErr
	}
	taskbarMessageName, err := windows.UTF16PtrFromString("TaskbarCreated")
	if err != nil {
		return err
	}
	taskbarCreated, _, taskbarErr := registerWindowMessageW.Call(uintptr(unsafe.Pointer(taskbarMessageName)))
	if taskbarCreated == 0 {
		return taskbarErr
	}
	state := &windowsTrayState{
		notify: windowsNotifyIcon{
			Size:            uint32(unsafe.Sizeof(windowsNotifyIcon{})),
			Window:          windows.Handle(window),
			ID:              1,
			Flags:           nifMessage | nifIcon | nifTip,
			CallbackMessage: archiveTrayCallbackMessage,
			Icon:            windows.Handle(icon),
		},
		taskbarCreated: uint32(taskbarCreated),
		openDashboard:  openDashboard,
	}
	copy(state.notify.Tip[:], windows.StringToUTF16("Archive"))
	activeWindowsTray = state
	defer func() { activeWindowsTray = nil }()
	if err := addWindowsTrayIcon(state); err != nil {
		return err
	}
	defer shellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&state.notify)))

	var message windowsMessage
	for {
		result, _, messageErr := getMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if result == ^uintptr(0) {
			return messageErr
		}
		if result == 0 {
			return nil
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&message)))
		dispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func addWindowsTrayIcon(state *windowsTrayState) error {
	result, _, callErr := shellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&state.notify)))
	if result == 0 {
		if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
			return callErr
		}
		return errors.New("Shell_NotifyIconW rejected Archive Tray")
	}
	return nil
}

func handleWindowsTrayMessage(window, message, parameter, detail uintptr) uintptr {
	state := activeWindowsTray
	if state != nil {
		switch uint32(message) {
		case archiveTrayCallbackMessage:
			if uint32(parameter) == state.notify.ID && uint32(detail) == wmLeftButtonUp {
				_ = state.openDashboard()
			}
			return 0
		case wmDestroy:
			postQuitMessage.Call(0)
			return 0
		default:
			if uint32(message) == state.taskbarCreated {
				_ = addWindowsTrayIcon(state)
				return 0
			}
		}
	}
	result, _, _ := defWindowProcW.Call(window, message, parameter, detail)
	return result
}
