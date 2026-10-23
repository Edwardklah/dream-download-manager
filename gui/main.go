package main

import (
	"embed"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// The built frontend (HTML, CSS, JS) is embedded into the .exe, so the app is
// a single file. Wails serves it to the window from memory.
//
//go:embed all:frontend/dist
var assets embed.FS

// The tray icon: the app's icon, simplified for 16-32 pixels.
//
//go:embed trayicon.png
var trayIcon []byte

// The app's icon in each theme's colours (for the tray and the window's
// taskbar button; the window's sidebar has its own copy).
//
//go:embed icons/*.png
var themeIcons embed.FS

var (
	tray      *application.SystemTray
	themeMu   sync.Mutex
	themeIcon []byte // the current theme's icon, for windows opened later
)

// defaultIcon is the Black theme's icon, the default.
func defaultIcon() []byte {
	icon, _ := themeIcons.ReadFile("icons/black.png")
	return icon
}

// applyTheme shows the theme's icon in the tray and on the window.
func applyTheme(theme string) {
	icon, err := themeIcons.ReadFile("icons/" + theme + ".png")
	if err != nil {
		return
	}
	themeMu.Lock()
	themeIcon = icon
	themeMu.Unlock()
	if tray != nil {
		tray.SetIcon(icon)
	}
	windowMu.Lock()
	w := window
	windowMu.Unlock()
	if w != nil {
		setWindowIcon(w, icon)
	}
}

// The app's design:
//
//   - The ENGINE and the TRAY ICON run all the time. That part is pure Go and
//     light.
//   - The WINDOW (WebView2, the browser engine that shows our interface) only
//     exists while it's open. Closing it DESTROYS it - not just hides it - so
//     its memory is given back. Clicking the tray icon creates a new one.
//
// Downloads carry on in the background whether or not the window is open.

func main() {
	svc := NewDownloadService()
	app := application.New(application.Options{
		Name:        "Dream Download Manager",
		Description: "A free, open-source download manager",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Windows: application.WindowsOptions{
			// Closing the window must NOT quit the app: downloads keep going
			// in the tray.
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true, // the same, on Linux
			// The window's program name, which desktops use to match it
			// with its menu entry (installer/linux/dreamdm.desktop).
			ProgramName: "dreamdm",
		},
		// The window's icon on Linux (Windows takes it from the program file).
		Icon: defaultIcon(),
		// Starting the app a second time just opens the existing one's window.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "io.github.dlmanager",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				showWindow()
			},
		},
	})

	tray = app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("Dream Download Manager")
	menu := app.NewMenu()
	menu.Add("Open").OnClick(func(*application.Context) { showWindow() })
	menu.AddSeparator()
	menu.Add("Pause all").OnClick(func(*application.Context) {
		if svc.manager != nil {
			svc.manager.PauseAll()
		}
	})
	menu.Add("Resume all").OnClick(func(*application.Context) {
		if svc.manager != nil {
			svc.manager.ResumeAll()
		}
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(showWindow)

	// "--tray" starts with no window (e.g. when starting with Windows);
	// otherwise open the window straight away.
	if !slices.Contains(os.Args[1:], "--tray") {
		showWindow()
	}

	// Diagnostics: if DLM_DIAG names a file, write memory statistics to it.
	if path := os.Getenv("DLM_DIAG"); path != "" {
		go writeDiagnostics(path)
	}

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

var (
	windowMu sync.Mutex
	window   *application.WebviewWindow // nil while the window is closed
)

// bringToFront shows the window ON TOP of whatever you're using - for a
// download that just arrived from the browser. Windows doesn't let a program
// in the background take the focus from the one you're using (so a window
// can't steal your typing), so a plain showWindow can open BEHIND the
// browser. Being "always on top" for a moment puts it in front without
// taking the focus; dlm-host also passes on the browser's permission to come
// to the front, when Windows allows it.
func bringToFront() {
	showWindow()
	windowMu.Lock()
	w := window
	windowMu.Unlock()
	if w == nil {
		return
	}
	w.SetAlwaysOnTop(true)
	time.AfterFunc(700*time.Millisecond, func() { w.SetAlwaysOnTop(false) })
}

// showWindow brings the window to the front, creating it if it's closed.
func showWindow() {
	windowMu.Lock()
	defer windowMu.Unlock()

	if window != nil {
		window.Show()
		window.UnMinimise()
		window.Focus()
		return
	}

	w := application.Get().Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Dream Download Manager",
		Width:            860, // compact: the sidebar shows its icons; wider shows it all
		Height:           640,
		MinWidth:         640,
		MinHeight:        460,
		BackgroundColour: application.NewRGB(17, 19, 24),
		URL:              "/",
	})
	window = w
	// The theme's icon on the window, once it exists on screen.
	w.OnWindowEvent(events.Common.WindowShow, func(*application.WindowEvent) {
		themeMu.Lock()
		icon := themeIcon
		themeMu.Unlock()
		if icon != nil {
			setWindowIcon(w, icon)
		}
	})

	// When the window closes it's destroyed; forget it, so the next
	// showWindow creates a fresh one.
	w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		windowMu.Lock()
		if window == w {
			window = nil
		}
		windowMu.Unlock()
		// The window's memory is now garbage. Hand it back to Windows soon,
		// rather than keeping it in reserve for later.
		go releaseMemorySoon()
	})
}
