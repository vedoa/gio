// SPDX-License-Identifier: Unlicense OR MIT

package app

import (
	"errors"
	"fmt"
	"image"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	syscall "golang.org/x/sys/windows"

	"gioui.org/app/internal/windows"
	"gioui.org/op"
	"gioui.org/unit"
	gowindows "golang.org/x/sys/windows"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/io/transfer"
)

type Win32ViewEvent struct {
	HWND uintptr
}

type window struct {
	hwnd syscall.Handle
	hdc  syscall.Handle
	w    *callbacks

	// cursorIn tracks whether the cursor was inside the window according
	// to the most recent WM_SETCURSOR.
	cursorIn bool
	cursor   syscall.Handle

	animating bool

	borderSize image.Point
	config     Config
	// frameDims stores the last seen window frame width and height.
	frameDims image.Point
	loop      *eventLoop
}

const _WM_WAKEUP = windows.WM_USER + iota

type gpuAPI struct {
	priority    int
	initializer func(w *window) (context, error)
}

// drivers is the list of potential Context implementations.
var drivers []gpuAPI

// winMap maps win32 HWNDs to *windows.
var winMap sync.Map

// iconID is the ID of the icon in the resource file.
const iconID = 1

var resources struct {
	once sync.Once
	// handle is the module handle from GetModuleHandle.
	handle syscall.Handle
	// class is the Gio window class from RegisterClassEx.
	class uint16
	// cursor is the arrow cursor resource.
	cursor syscall.Handle
}

func osMain() {
	select {}
}

func newWindow(win *callbacks, options []Option) {
	done := make(chan struct{})
	go func() {
		// GetMessage and PeekMessage can filter on a window HWND, but
		// then thread-specific messages such as WM_QUIT are ignored.
		// Instead lock the thread so window messages arrive through
		// unfiltered GetMessage calls.
		runtime.LockOSThread()

		w := &window{
			w: win,
		}
		w.loop = newEventLoop(w.w, w.wakeup)
		w.w.SetDriver(w)
		err := w.init()
		done <- struct{}{}
		if err != nil {
			w.ProcessEvent(DestroyEvent{Err: err})
			return
		}
		winMap.Store(w.hwnd, w)
		defer winMap.Delete(w.hwnd)
		w.Configure(options)
		w.ProcessEvent(Win32ViewEvent{HWND: uintptr(w.hwnd)})
		windows.SetForegroundWindow(w.hwnd)
		windows.SetFocus(w.hwnd)
		// Since the window class for the cursor is null,
		// set it here to show the cursor.
		w.SetCursor(pointer.CursorDefault)
		w.runLoop()
	}()
	<-done
}

// initResources initializes the resources global.
func initResources() error {
	windows.SetProcessDPIAware()
	hInst, err := windows.GetModuleHandle()
	if err != nil {
		return err
	}
	resources.handle = hInst
	c, err := windows.LoadCursor(windows.IDC_ARROW)
	if err != nil {
		return err
	}
	resources.cursor = c
	icon, _ := windows.LoadImage(hInst, iconID, windows.IMAGE_ICON, 0, 0, windows.LR_DEFAULTSIZE|windows.LR_SHARED)
	wcls := windows.WndClassEx{
		CbSize:        uint32(unsafe.Sizeof(windows.WndClassEx{})),
		Style:         windows.CS_HREDRAW | windows.CS_VREDRAW | windows.CS_OWNDC,
		LpfnWndProc:   syscall.NewCallback(windowProc),
		HInstance:     hInst,
		HIcon:         icon,
		LpszClassName: syscall.StringToUTF16Ptr("GioWindow"),
	}
	cls, err := windows.RegisterClassEx(&wcls)
	if err != nil {
		return err
	}
	resources.class = cls
	return nil
}

const dwExStyle = windows.WS_EX_APPWINDOW | windows.WS_EX_WINDOWEDGE

func (w *window) init() error {
	var resErr error
	resources.once.Do(func() {
		resErr = initResources()
	})
	if resErr != nil {
		return resErr
	}
	const dwStyle = windows.WS_OVERLAPPEDWINDOW

	hwnd, err := windows.CreateWindowEx(
		dwExStyle,
		resources.class,
		"",
		dwStyle|windows.WS_CLIPSIBLINGS|windows.WS_CLIPCHILDREN,
		windows.CW_USEDEFAULT, windows.CW_USEDEFAULT,
		windows.CW_USEDEFAULT, windows.CW_USEDEFAULT,
		0,
		0,
		resources.handle,
		0)
	if err != nil {
		return err
	}
	if err := windows.RegisterTouchWindow(hwnd, 0); err != nil {
		return err
	}
	if err := windows.EnableMouseInPointer(1); err != nil {
		return err
	}
	w.hdc, err = windows.GetDC(hwnd)
	if err != nil {
		windows.DestroyWindow(hwnd)
		return err
	}
	w.hwnd = hwnd
	return nil
}

// update handles changes done by the user, and updates the configuration.
// It reads the window style and size/position and updates w.config.
// If anything has changed it emits a ConfigEvent to notify the application.
func (w *window) update() {
	p := windows.GetWindowPlacement(w.hwnd)
	if !p.IsMinimized() {
		r := windows.GetWindowRect(w.hwnd)
		cr := windows.GetClientRect(w.hwnd)
		w.config.Size = image.Point{
			X: int(cr.Right - cr.Left),
			Y: int(cr.Bottom - cr.Top),
		}
		w.frameDims = image.Point{
			X: int(r.Right - r.Left),
			Y: int(r.Bottom - r.Top),
		}.Sub(w.config.Size)
	}

	w.borderSize = image.Pt(
		windows.GetSystemMetrics(windows.SM_CXSIZEFRAME),
		windows.GetSystemMetrics(windows.SM_CYSIZEFRAME),
	)
	style := windows.GetWindowLong(w.hwnd, windows.GWL_STYLE)
	switch {
	case p.IsMaximized() && style&windows.WS_OVERLAPPEDWINDOW != 0:
		w.config.Mode = Maximized
	case p.IsMaximized():
		w.config.Mode = Fullscreen
	default:
		w.config.Mode = Windowed
	}
	w.ProcessEvent(ConfigEvent{Config: w.config})
	w.draw(true)
}

func windowProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	win, exists := winMap.Load(hwnd)
	if !exists {
		return windows.DefWindowProc(hwnd, msg, wParam, lParam)
	}

	w := win.(*window)

	switch msg {
	case windows.WM_UNICHAR:
		if wParam == windows.UNICODE_NOCHAR {
			// Tell the system that we accept WM_UNICHAR messages.
			return windows.TRUE
		}
		fallthrough
	case windows.WM_CHAR:
		if r := rune(wParam); unicode.IsPrint(r) {
			w.w.EditorInsert(string(r))
		}
		// The message is processed.
		return windows.TRUE
	case windows.WM_DPICHANGED:
		// Let Windows know we're prepared for runtime DPI changes.
		return windows.TRUE
	case windows.WM_ERASEBKGND:
		// Avoid flickering between GPU content and background color.
		return windows.TRUE
	case windows.WM_KEYDOWN, windows.WM_KEYUP, windows.WM_SYSKEYDOWN, windows.WM_SYSKEYUP:
		if n, ok := convertKeyCode(wParam); ok {
			e := key.Event{
				Name:      n,
				Modifiers: getModifiers(),
				State:     key.Press,
			}
			if msg == windows.WM_KEYUP || msg == windows.WM_SYSKEYUP {
				e.State = key.Release
			}

			w.ProcessEvent(e)

			if (wParam == windows.VK_F10) && (msg == windows.WM_SYSKEYDOWN || msg == windows.WM_SYSKEYUP) {
				// Reserve F10 for ourselves, and don't let it open the system menu. Other Windows programs
				// such as cmd.exe and graphical debuggers also reserve F10.
				return 0
			}
		}
	case windows.WM_POINTERDOWN, windows.WM_POINTERUP, windows.WM_POINTERUPDATE, windows.WM_POINTERCAPTURECHANGED:
		pid := getPointerIDwParam(wParam)
		pi, err := windows.GetPointerInfo(uint32(pid))
		if err != nil {
			panic(err)
		}
		switch msg {
		case windows.WM_POINTERDOWN:
			windows.SetCapture(w.hwnd)
		case windows.WM_POINTERUP:
			windows.ReleaseCapture()
		}

		kind := pointer.Move
		switch pi.ButtonChangeType {
		case windows.POINTER_CHANGE_FIRSTBUTTON_DOWN, windows.POINTER_CHANGE_SECONDBUTTON_DOWN, windows.POINTER_CHANGE_THIRDBUTTON_DOWN, windows.POINTER_CHANGE_FOURTHBUTTON_DOWN, windows.POINTER_CHANGE_FIFTHBUTTON_DOWN:
			kind = pointer.Press
		case windows.POINTER_CHANGE_FIRSTBUTTON_UP, windows.POINTER_CHANGE_SECONDBUTTON_UP, windows.POINTER_CHANGE_THIRDBUTTON_UP, windows.POINTER_CHANGE_FOURTHBUTTON_UP, windows.POINTER_CHANGE_FIFTHBUTTON_UP:
			kind = pointer.Release
		}

		if (pi.PointerFlags&windows.POINTER_FLAG_CANCELED != 0) || (msg == windows.WM_POINTERCAPTURECHANGED) {
			kind = pointer.Cancel
		}

		w.pointerUpdate(pi, pid, kind, lParam)
	case windows.WM_CANCELMODE:
		w.ProcessEvent(pointer.Event{
			Kind: pointer.Cancel,
		})
	case windows.WM_SETFOCUS:
		w.config.Focused = true
		w.ProcessEvent(ConfigEvent{Config: w.config})
	case windows.WM_KILLFOCUS:
		w.config.Focused = false
		w.ProcessEvent(ConfigEvent{Config: w.config})
	case windows.WM_NCHITTEST:
		if w.config.Decorated {
			// Let the system handle it.
			break
		}
		x, y := coordsFromlParam(lParam)
		np := windows.Point{X: int32(x), Y: int32(y)}
		windows.ScreenToClient(w.hwnd, &np)
		return w.hitTest(int(np.X), int(np.Y))
	case windows.WM_POINTERWHEEL:
		w.scrollEvent(wParam, lParam, false, getModifiers())
	case windows.WM_POINTERHWHEEL:
		w.scrollEvent(wParam, lParam, true, getModifiers())
	case windows.WM_DESTROY:
		w.ProcessEvent(Win32ViewEvent{})
		w.ProcessEvent(DestroyEvent{})
		w.w = nil
		if w.hdc != 0 {
			windows.ReleaseDC(w.hdc)
			w.hdc = 0
		}
		// The system destroys the HWND for us.
		w.hwnd = 0
		windows.PostQuitMessage(0)
		return 0
	case windows.WM_NCCALCSIZE:
		if w.config.Decorated {
			// Let Windows handle decorations.
			break
		}
		// No client areas; we draw decorations ourselves.
		if wParam != 1 {
			return 0
		}
		// lParam contains an NCCALCSIZE_PARAMS for us to adjust.
		place := windows.GetWindowPlacement(w.hwnd)
		if !place.IsMaximized() {
			// Nothing do adjust.
			return 0
		}
		// Adjust window position to avoid the extra padding in maximized
		// state. See https://devblogs.microsoft.com/oldnewthing/20150304-00/?p=44543.
		// Note that trying to do the adjustment in WM_GETMINMAXINFO is ignored by Windows.
		szp := (*windows.NCCalcSizeParams)(unsafe.Pointer(lParam))
		mi := windows.GetMonitorInfo(w.hwnd)
		szp.Rgrc[0] = mi.WorkArea
		return 0
	case windows.WM_PAINT:
		w.draw(true)
	case windows.WM_STYLECHANGED:
		w.update()
	case windows.WM_WINDOWPOSCHANGED:
		w.update()
	case windows.WM_SIZE:
		w.update()
	case windows.WM_GETMINMAXINFO:
		mm := (*windows.MinMaxInfo)(unsafe.Pointer(lParam))

		var frameDims image.Point
		if w.config.Decorated {
			frameDims = w.frameDims
		}
		if p := w.config.MinSize; p.X > 0 || p.Y > 0 {
			p = p.Add(frameDims)
			mm.PtMinTrackSize = windows.Point{
				X: int32(p.X),
				Y: int32(p.Y),
			}
		}
		if p := w.config.MaxSize; p.X > 0 || p.Y > 0 {
			p = p.Add(frameDims)
			mm.PtMaxTrackSize = windows.Point{
				X: int32(p.X),
				Y: int32(p.Y),
			}
		}
		return 0
	case windows.WM_SETCURSOR:
		w.cursorIn = (lParam & 0xffff) == windows.HTCLIENT
		if w.cursorIn {
			windows.SetCursor(w.cursor)
			return windows.TRUE
		}
	case _WM_WAKEUP:
		w.loop.Wakeup()
		w.loop.FlushEvents()
	case windows.WM_IME_STARTCOMPOSITION:
		imc := windows.ImmGetContext(w.hwnd)
		if imc == 0 {
			return windows.TRUE
		}
		defer windows.ImmReleaseContext(w.hwnd, imc)
		sel := w.w.EditorState().Selection
		caret := sel.Transform.Transform(sel.Caret.Pos.Add(f32.Pt(0, sel.Caret.Descent)))
		icaret := image.Pt(int(caret.X+.5), int(caret.Y+.5))
		windows.ImmSetCompositionWindow(imc, icaret.X, icaret.Y)
		windows.ImmSetCandidateWindow(imc, icaret.X, icaret.Y)
	case windows.WM_IME_COMPOSITION:
		imc := windows.ImmGetContext(w.hwnd)
		if imc == 0 {
			return windows.TRUE
		}
		defer windows.ImmReleaseContext(w.hwnd, imc)
		state := w.w.EditorState()
		rng := state.compose
		if rng.Start == -1 {
			rng = state.Selection.Range
		}
		if rng.Start > rng.End {
			rng.Start, rng.End = rng.End, rng.Start
		}
		var replacement string
		switch {
		case lParam&windows.GCS_RESULTSTR != 0:
			replacement = windows.ImmGetCompositionString(imc, windows.GCS_RESULTSTR)
		case lParam&windows.GCS_COMPSTR != 0:
			replacement = windows.ImmGetCompositionString(imc, windows.GCS_COMPSTR)
		}
		end := rng.Start + utf8.RuneCountInString(replacement)
		w.w.EditorReplace(rng, replacement)
		state = w.w.EditorState()
		comp := key.Range{
			Start: rng.Start,
			End:   end,
		}
		if lParam&windows.GCS_DELTASTART != 0 {
			start := windows.ImmGetCompositionValue(imc, windows.GCS_DELTASTART)
			comp.Start = state.RunesIndex(state.UTF16Index(comp.Start) + start)
		}
		w.w.SetComposingRegion(comp)
		pos := end
		if lParam&windows.GCS_CURSORPOS != 0 {
			rel := windows.ImmGetCompositionValue(imc, windows.GCS_CURSORPOS)
			pos = state.RunesIndex(state.UTF16Index(rng.Start) + rel)
		}
		w.w.SetEditorSelection(key.Range{Start: pos, End: pos})
		return windows.TRUE
	case windows.WM_IME_ENDCOMPOSITION:
		w.w.SetComposingRegion(key.Range{Start: -1, End: -1})
		return windows.TRUE
	}

	return windows.DefWindowProc(hwnd, msg, wParam, lParam)
}

func getModifiers() key.Modifiers {
	var kmods key.Modifiers
	if windows.GetKeyState(windows.VK_LWIN)&0x1000 != 0 || windows.GetKeyState(windows.VK_RWIN)&0x1000 != 0 {
		kmods |= key.ModSuper
	}
	if windows.GetKeyState(windows.VK_MENU)&0x1000 != 0 {
		kmods |= key.ModAlt
	}
	if windows.GetKeyState(windows.VK_CONTROL)&0x1000 != 0 {
		kmods |= key.ModCtrl
	}
	if windows.GetKeyState(windows.VK_SHIFT)&0x1000 != 0 {
		kmods |= key.ModShift
	}
	return kmods
}

// hitTest returns the non-client area hit by the point, needed to
// process WM_NCHITTEST.
func (w *window) hitTest(x, y int) uintptr {
	if w.config.Mode != Windowed {
		// Only windowed mode should allow resizing.
		return windows.HTCLIENT
	}
	// Check for resize handle before system actions; otherwise it can be impossible to
	// resize a custom-decorations window when the system move area is flush with the
	// edge of the window.
	top := y <= w.borderSize.Y
	bottom := y >= w.config.Size.Y-w.borderSize.Y
	left := x <= w.borderSize.X
	right := x >= w.config.Size.X-w.borderSize.X
	switch {
	case top && left:
		return windows.HTTOPLEFT
	case top && right:
		return windows.HTTOPRIGHT
	case bottom && left:
		return windows.HTBOTTOMLEFT
	case bottom && right:
		return windows.HTBOTTOMRIGHT
	case top:
		return windows.HTTOP
	case bottom:
		return windows.HTBOTTOM
	case left:
		return windows.HTLEFT
	case right:
		return windows.HTRIGHT
	}
	p := f32.Pt(float32(x), float32(y))
	if a, ok := w.w.ActionAt(p); ok && a == system.ActionMove {
		return windows.HTCAPTION
	}
	return windows.HTCLIENT
}

func (w *window) pointerUpdate(pi windows.PointerInfo, pid pointer.ID, kind pointer.Kind, lParam uintptr) {
	if !w.config.Focused {
		windows.SetFocus(w.hwnd)
	}

	src := pointer.Touch
	if pi.PointerType == windows.PT_MOUSE {
		src = pointer.Mouse
	}

	x, y := coordsFromlParam(lParam)
	np := windows.Point{X: int32(x), Y: int32(y)}
	windows.ScreenToClient(w.hwnd, &np)
	p := f32.Point{X: float32(np.X), Y: float32(np.Y)}
	w.ProcessEvent(pointer.Event{
		Kind:      kind,
		Source:    src,
		Position:  p,
		PointerID: pid,
		Buttons:   getPointerButtons(pi),
		Time:      windows.GetMessageTime(),
		Modifiers: getModifiers(),
	})
}

func coordsFromlParam(lParam uintptr) (int, int) {
	x := int(int16(lParam & 0xffff))
	y := int(int16((lParam >> 16) & 0xffff))
	return x, y
}

func (w *window) scrollEvent(wParam, lParam uintptr, horizontal bool, kmods key.Modifiers) {
	pid := getPointerIDwParam(wParam)
	pi, err := windows.GetPointerInfo(uint32(pid))
	if err != nil {
		panic(err)
	}

	x, y := coordsFromlParam(lParam)
	// The WM_MOUSEWHEEL coordinates are in screen coordinates, in contrast
	// to other mouse events.
	np := windows.Point{X: int32(x), Y: int32(y)}
	windows.ScreenToClient(w.hwnd, &np)
	p := f32.Point{X: float32(np.X), Y: float32(np.Y)}
	dist := float32(int16(wParam >> 16))
	var sp f32.Point
	if horizontal {
		sp.X = dist
	} else {
		// support horizontal scroll (shift + mousewheel)
		if kmods == key.ModShift {
			sp.X = -dist
		} else {
			sp.Y = -dist
		}
	}
	w.ProcessEvent(pointer.Event{
		Kind:      pointer.Scroll,
		Source:    pointer.Mouse,
		Position:  p,
		Buttons:   getPointerButtons(pi),
		Scroll:    sp,
		Modifiers: kmods,
		Time:      windows.GetMessageTime(),
	})
}

// Adapted from https://blogs.msdn.microsoft.com/oldnewthing/20060126-00/?p=32513/
func (w *window) runLoop() {
	msg := new(windows.Msg)
loop:
	for {
		anim := w.animating
		p := windows.GetWindowPlacement(w.hwnd)
		if anim && !p.IsMinimized() && !windows.PeekMessage(msg, 0, 0, 0, windows.PM_NOREMOVE) {
			w.draw(false)
			continue
		}
		switch ret := windows.GetMessage(msg, 0, 0, 0); ret {
		case -1:
			panic(errors.New("GetMessage failed"))
		case 0:
			// WM_QUIT received.
			break loop
		}
		windows.TranslateMessage(msg)
		windows.DispatchMessage(msg)
	}
}

func (w *window) EditorStateChanged(old, new editorState) {
	imc := windows.ImmGetContext(w.hwnd)
	if imc == 0 {
		return
	}
	defer windows.ImmReleaseContext(w.hwnd, imc)
	if old.Selection.Range != new.Selection.Range || old.Snippet != new.Snippet {
		windows.ImmNotifyIME(imc, windows.NI_COMPOSITIONSTR, windows.CPS_CANCEL, 0)
	}
}

func (w *window) SetAnimating(anim bool) {
	w.animating = anim
}

func (w *window) ProcessEvent(e event.Event) {
	w.w.ProcessEvent(e)
	w.loop.FlushEvents()
}

func (w *window) Event() event.Event {
	return w.loop.Event()
}

func (w *window) Invalidate() {
	w.loop.Invalidate()
}

func (w *window) Run(f func()) {
	w.loop.Run(f)
}

func (w *window) Frame(frame *op.Ops) {
	w.loop.Frame(frame)
}

func (w *window) wakeup() {
	if err := windows.PostMessage(w.hwnd, _WM_WAKEUP, 0, 0); err != nil {
		panic(err)
	}
}

func (w *window) draw(sync bool) {
	if w.config.Size.X == 0 || w.config.Size.Y == 0 {
		return
	}
	dpi := windows.GetWindowDPI(w.hwnd)
	cfg := configForDPI(dpi)
	w.ProcessEvent(frameEvent{
		FrameEvent: FrameEvent{
			Now:    time.Now(),
			Size:   w.config.Size,
			Metric: cfg,
		},
		Sync: sync,
	})
}

func (w *window) NewContext() (context, error) {
	sort.Slice(drivers, func(i, j int) bool {
		return drivers[i].priority < drivers[j].priority
	})
	var errs []string
	for _, b := range drivers {
		ctx, err := b.initializer(w)
		if err == nil {
			return ctx, nil
		}
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("NewContext: failed to create a GPU device, tried: %s", strings.Join(errs, ", "))
	}
	return nil, errors.New("NewContext: no available GPU drivers")
}

func (w *window) ReadClipboard() {
	w.readClipboard()
}

func (w *window) readClipboard() error {
	if err := windows.OpenClipboard(w.hwnd); err != nil {
		return err
	}
	defer windows.CloseClipboard()
	mem, err := windows.GetClipboardData(windows.CF_UNICODETEXT)
	if err != nil {
		return err
	}
	ptr, err := windows.GlobalLock(mem)
	if err != nil {
		return err
	}
	defer windows.GlobalUnlock(mem)
	content := gowindows.UTF16PtrToString((*uint16)(unsafe.Pointer(ptr)))
	w.ProcessEvent(transfer.DataEvent{
		Type: "application/text",
		Open: func() io.ReadCloser {
			return io.NopCloser(strings.NewReader(content))
		},
	})
	return nil
}

func (w *window) Configure(options []Option) {
	dpi := windows.GetSystemDPI()
	metric := configForDPI(dpi)
	cnf := w.config
	cnf.apply(metric, options)
	w.config.Title = cnf.Title
	w.config.Decorated = cnf.Decorated
	w.config.MinSize = cnf.MinSize
	w.config.MaxSize = cnf.MaxSize
	windows.SetWindowText(w.hwnd, cnf.Title)

	style := windows.GetWindowLong(w.hwnd, windows.GWL_STYLE)
	var showMode int32
	var x, y, width, height int32
	swpStyle := uintptr(windows.SWP_NOZORDER | windows.SWP_FRAMECHANGED)
	winStyle := uintptr(windows.WS_OVERLAPPEDWINDOW)
	style &^= winStyle
	switch cnf.Mode {
	case Minimized:
		style |= winStyle
		swpStyle |= windows.SWP_NOMOVE | windows.SWP_NOSIZE
		showMode = windows.SW_SHOWMINIMIZED

	case Maximized:
		style |= winStyle
		swpStyle |= windows.SWP_NOMOVE | windows.SWP_NOSIZE
		showMode = windows.SW_SHOWMAXIMIZED

	case Windowed:
		style |= winStyle
		showMode = windows.SW_SHOWNORMAL
		// Get target for client area size.
		width = int32(cnf.Size.X)
		height = int32(cnf.Size.Y)
		// Get the current window size and position.
		wr := windows.GetWindowRect(w.hwnd)
		x = wr.Left
		y = wr.Top
		if cnf.Decorated {
			// Compute client size and position. Note that the client size is
			// equal to the window size when we are in control of decorations.
			r := windows.Rect{
				Right:  width,
				Bottom: height,
			}
			windows.AdjustWindowRectEx(&r, uint32(style), 0, dwExStyle)
			width = r.Right - r.Left
			height = r.Bottom - r.Top
		} else {
			// Enable drop shadows when we draw decorations.
			windows.DwmExtendFrameIntoClientArea(w.hwnd, windows.Margins{-1, -1, -1, -1})
		}

	case Fullscreen:
		swpStyle |= windows.SWP_NOMOVE | windows.SWP_NOSIZE
		showMode = windows.SW_SHOWMAXIMIZED
	}

	// Disable window resizing if MinSize and MaxSize are equal.
	if cnf.MaxSize != (image.Point{}) && cnf.MinSize == cnf.MaxSize {
		style &^= windows.WS_MAXIMIZEBOX
		style &^= windows.WS_THICKFRAME
	}

	// Note: these invocation all trigger the windows callback method which may process a pending system.ActionCenter
	// action, so SetWindowPos should come first so as to not "overwrite" system.ActionCenter.
	windows.SetWindowPos(w.hwnd, 0, x, y, width, height, swpStyle)
	windows.SetWindowLong(w.hwnd, windows.GWL_STYLE, style)
	windows.ShowWindow(w.hwnd, showMode)
}

func (w *window) WriteClipboard(mime string, s []byte) {
	w.writeClipboard(string(s))
}

func (w *window) writeClipboard(s string) error {
	if err := windows.OpenClipboard(w.hwnd); err != nil {
		return err
	}
	defer windows.CloseClipboard()
	if err := windows.EmptyClipboard(); err != nil {
		return err
	}
	u16, err := gowindows.UTF16FromString(s)
	if err != nil {
		return err
	}
	n := len(u16) * int(unsafe.Sizeof(u16[0]))
	mem, err := windows.GlobalAlloc(n)
	if err != nil {
		return err
	}
	ptr, err := windows.GlobalLock(mem)
	if err != nil {
		windows.GlobalFree(mem)
		return err
	}
	u16v := unsafe.Slice((*uint16)(ptr), len(u16))
	copy(u16v, u16)
	windows.GlobalUnlock(mem)
	if err := windows.SetClipboardData(windows.CF_UNICODETEXT, mem); err != nil {
		windows.GlobalFree(mem)
		return err
	}
	return nil
}

func (w *window) SetCursor(cursor pointer.Cursor) {
	c, err := loadCursor(cursor)
	if err != nil {
		c = resources.cursor
	}
	w.cursor = c
	if w.cursorIn {
		windows.SetCursor(w.cursor)
	}
}

// windowsCursor contains mapping from pointer.Cursor to an IDC.
var windowsCursor = [...]uint16{
	pointer.CursorDefault:                  windows.IDC_ARROW,
	pointer.CursorNone:                     0,
	pointer.CursorText:                     windows.IDC_IBEAM,
	pointer.CursorVerticalText:             windows.IDC_IBEAM,
	pointer.CursorPointer:                  windows.IDC_HAND,
	pointer.CursorCrosshair:                windows.IDC_CROSS,
	pointer.CursorAllScroll:                windows.IDC_SIZEALL,
	pointer.CursorColResize:                windows.IDC_SIZEWE,
	pointer.CursorRowResize:                windows.IDC_SIZENS,
	pointer.CursorGrab:                     windows.IDC_SIZEALL,
	pointer.CursorGrabbing:                 windows.IDC_SIZEALL,
	pointer.CursorNotAllowed:               windows.IDC_NO,
	pointer.CursorWait:                     windows.IDC_WAIT,
	pointer.CursorProgress:                 windows.IDC_APPSTARTING,
	pointer.CursorNorthWestResize:          windows.IDC_SIZENWSE,
	pointer.CursorNorthEastResize:          windows.IDC_SIZENESW,
	pointer.CursorSouthWestResize:          windows.IDC_SIZENESW,
	pointer.CursorSouthEastResize:          windows.IDC_SIZENWSE,
	pointer.CursorNorthSouthResize:         windows.IDC_SIZENS,
	pointer.CursorEastWestResize:           windows.IDC_SIZEWE,
	pointer.CursorWestResize:               windows.IDC_SIZEWE,
	pointer.CursorEastResize:               windows.IDC_SIZEWE,
	pointer.CursorNorthResize:              windows.IDC_SIZENS,
	pointer.CursorSouthResize:              windows.IDC_SIZENS,
	pointer.CursorNorthEastSouthWestResize: windows.IDC_SIZENESW,
	pointer.CursorNorthWestSouthEastResize: windows.IDC_SIZENWSE,
}

func loadCursor(cursor pointer.Cursor) (syscall.Handle, error) {
	switch cursor {
	case pointer.CursorDefault:
		return resources.cursor, nil
	case pointer.CursorNone:
		return 0, nil
	default:
		return windows.LoadCursor(windowsCursor[cursor])
	}
}

func (w *window) ShowTextInput(show bool) {}

func (w *window) SetInputHint(_ key.InputHint) {}

func (w *window) HDC() syscall.Handle {
	return w.hdc
}

func (w *window) HWND() (syscall.Handle, int, int) {
	return w.hwnd, w.config.Size.X, w.config.Size.Y
}

func (w *window) Perform(acts system.Action) {
	walkActions(acts, func(a system.Action) {
		switch a {
		case system.ActionCenter:
			if w.config.Mode != Windowed {
				break
			}
			r := windows.GetWindowRect(w.hwnd)
			dx := r.Right - r.Left
			dy := r.Bottom - r.Top
			// Calculate center position on current monitor.
			mi := windows.GetMonitorInfo(w.hwnd).Monitor
			x := (mi.Right - mi.Left - dx) / 2
			y := (mi.Bottom - mi.Top - dy) / 2
			windows.SetWindowPos(w.hwnd, 0, x, y, dx, dy, windows.SWP_NOZORDER|windows.SWP_FRAMECHANGED)
		case system.ActionRaise:
			w.raise()
		case system.ActionClose:
			windows.PostMessage(w.hwnd, windows.WM_CLOSE, 0, 0)
		}
	})
}

func (w *window) raise() {
	windows.SetForegroundWindow(w.hwnd)
	windows.SetWindowPos(w.hwnd, windows.HWND_TOPMOST, 0, 0, 0, 0,
		windows.SWP_NOMOVE|windows.SWP_NOSIZE|windows.SWP_SHOWWINDOW)
}

func convertKeyCode(code uintptr) (key.Name, bool) {
	if '0' <= code && code <= '9' || 'A' <= code && code <= 'Z' {
		return key.Name(rune(code)), true
	}
	var r key.Name

	switch code {
	case windows.VK_ESCAPE:
		r = key.NameEscape
	case windows.VK_LEFT:
		r = key.NameLeftArrow
	case windows.VK_RIGHT:
		r = key.NameRightArrow
	case windows.VK_RETURN:
		r = key.NameReturn
	case windows.VK_UP:
		r = key.NameUpArrow
	case windows.VK_DOWN:
		r = key.NameDownArrow
	case windows.VK_HOME:
		r = key.NameHome
	case windows.VK_END:
		r = key.NameEnd
	case windows.VK_BACK:
		r = key.NameDeleteBackward
	case windows.VK_DELETE:
		r = key.NameDeleteForward
	case windows.VK_PRIOR:
		r = key.NamePageUp
	case windows.VK_NEXT:
		r = key.NamePageDown
	case windows.VK_F1:
		r = key.NameF1
	case windows.VK_F2:
		r = key.NameF2
	case windows.VK_F3:
		r = key.NameF3
	case windows.VK_F4:
		r = key.NameF4
	case windows.VK_F5:
		r = key.NameF5
	case windows.VK_F6:
		r = key.NameF6
	case windows.VK_F7:
		r = key.NameF7
	case windows.VK_F8:
		r = key.NameF8
	case windows.VK_F9:
		r = key.NameF9
	case windows.VK_F10:
		r = key.NameF10
	case windows.VK_F11:
		r = key.NameF11
	case windows.VK_F12:
		r = key.NameF12
	case windows.VK_TAB:
		r = key.NameTab
	case windows.VK_SPACE:
		r = key.NameSpace
	case windows.VK_OEM_1:
		r = ";"
	case windows.VK_OEM_PLUS:
		r = "+"
	case windows.VK_OEM_COMMA:
		r = ","
	case windows.VK_OEM_MINUS:
		r = "-"
	case windows.VK_OEM_PERIOD:
		r = "."
	case windows.VK_OEM_2:
		r = "/"
	case windows.VK_OEM_3:
		r = "`"
	case windows.VK_OEM_4:
		r = "["
	case windows.VK_OEM_5, windows.VK_OEM_102:
		r = "\\"
	case windows.VK_OEM_6:
		r = "]"
	case windows.VK_OEM_7:
		r = "'"
	case windows.VK_CONTROL:
		r = key.NameCtrl
	case windows.VK_SHIFT:
		r = key.NameShift
	case windows.VK_MENU:
		r = key.NameAlt
	case windows.VK_LWIN, windows.VK_RWIN:
		r = key.NameSuper
	default:
		return "", false
	}
	return r, true
}

func configForDPI(dpi int) unit.Metric {
	const inchPrDp = 1.0 / 96.0
	ppdp := float32(dpi) * inchPrDp
	return unit.Metric{
		PxPerDp: ppdp,
		PxPerSp: ppdp,
	}
}

func (Win32ViewEvent) implementsViewEvent() {}
func (Win32ViewEvent) ImplementsEvent()     {}
func (w Win32ViewEvent) Valid() bool {
	return w != (Win32ViewEvent{})
}

// LOWORD (minwindef.h)
func loWord(val uint32) uint16 {
	return uint16(val & 0xFFFF)
}

// GET_POINTERID_WPARAM (winuser.h)
func getPointerIDwParam(wParam uintptr) pointer.ID {
	return pointer.ID(loWord(uint32(wParam)))
}

func getPointerButtons(pi windows.PointerInfo) pointer.Buttons {
	var btns pointer.Buttons

	if pi.PointerFlags&windows.POINTER_FLAG_FIRSTBUTTON != 0 {
		btns |= pointer.ButtonPrimary
	} else {
		btns &^= pointer.ButtonPrimary
	}
	if pi.PointerFlags&windows.POINTER_FLAG_SECONDBUTTON != 0 {
		btns |= pointer.ButtonSecondary
	} else {
		btns &^= pointer.ButtonSecondary
	}
	if pi.PointerFlags&windows.POINTER_FLAG_THIRDBUTTON != 0 {
		btns |= pointer.ButtonTertiary
	} else {
		btns &^= pointer.ButtonTertiary
	}
	if pi.PointerFlags&windows.POINTER_FLAG_FOURTHBUTTON != 0 {
		btns |= pointer.ButtonQuaternary
	} else {
		btns &^= pointer.ButtonQuaternary
	}
	if pi.PointerFlags&windows.POINTER_FLAG_FIFTHBUTTON != 0 {
		btns |= pointer.ButtonQuinary
	} else {
		btns &^= pointer.ButtonQuinary
	}

	return btns
}
