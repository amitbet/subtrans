package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	wailsoptions "github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

var version = "dev"

const ffmpegStaticVersion = "b6.1.2-rc.1"

//go:embed all:frontend/dist
var frontendAssets embed.FS

var videoExtensions = map[string]bool{
	".3gp":  true,
	".avi":  true,
	".flv":  true,
	".m4v":  true,
	".mkv":  true,
	".mov":  true,
	".mp4":  true,
	".mpeg": true,
	".mpg":  true,
	".ts":   true,
	".webm": true,
	".wmv":  true,
}

type options struct {
	targetLang string
	sourceLang string
	recursive  bool
	overwrite  bool
	register   bool
	cli        bool
	minSize    int64
	timeout    time.Duration
	path       string
}

type cue struct {
	Prefix []string
	Timing string
	Text   []string
}

type translateClient struct {
	httpClient *http.Client
	source     string
	target     string
}

func main() {
	if shouldRunGUI(os.Args[1:]) {
		runGUI(os.Args[1:])
		return
	}
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func shouldRunGUI(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-cli", "--cli", "-register", "--register", "-version", "--version", "-h", "--help":
			return false
		}
		if strings.HasPrefix(arg, "-cli=") || strings.HasPrefix(arg, "--cli=") ||
			strings.HasPrefix(arg, "-register=") || strings.HasPrefix(arg, "--register=") ||
			strings.HasPrefix(arg, "-version=") || strings.HasPrefix(arg, "--version=") {
			return false
		}
	}
	return true
}

func runGUI(args []string) {
	guiApp := newWailsGUI(args)
	if err := wails.Run(&wailsoptions.App{
		Title:     "Subtrans",
		Width:     980,
		Height:    680,
		MinWidth:  820,
		MinHeight: 560,
		AssetServer: &assetserver.Options{
			Assets: frontendAssets,
		},
		OnStartup: guiApp.startup,
		DragAndDrop: &wailsoptions.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: true,
		},
		BackgroundColour: wailsoptions.NewRGB(246, 247, 251),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type runEvents struct {
	setQueue     func([]string)
	markActive   func(string)
	removeQueued func(string)
	control      *queueControl
}

// queueControl lets the frontend pause/resume the queue and remove individual
// videos while runWithEvents is processing them. It is nil for the CLI path.
type queueControl struct {
	mu           sync.Mutex
	cond         *sync.Cond
	paused       bool
	removed      map[string]bool
	curPath      string
	cancel       context.CancelFunc
	cancelReason string // "pause" | "remove" | ""
	emitRemoved  func(string)
	emitPaused   func(bool)
}

func newQueueControl(emitRemoved func(string), emitPaused func(bool)) *queueControl {
	q := &queueControl{
		removed:     map[string]bool{},
		emitRemoved: emitRemoved,
		emitPaused:  emitPaused,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// begin blocks while the queue is paused, then returns a context for processing
// path. proceed is false when the user removed the item while it was queued.
func (q *queueControl) begin(parent context.Context, path string) (context.Context, bool) {
	q.mu.Lock()
	for q.paused && !q.removed[path] {
		q.cond.Wait()
	}
	if q.removed[path] {
		q.mu.Unlock()
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	q.curPath = path
	q.cancel = cancel
	q.cancelReason = ""
	q.mu.Unlock()
	return ctx, true
}

// done finalizes the current item and reports why it was cancelled, if at all.
func (q *queueControl) done() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.curPath = ""
	reason := q.cancelReason
	q.cancelReason = ""
	return reason
}

// toggle pauses or resumes the queue. Pausing cancels the in-flight video so it
// stops promptly; it is re-processed from the start when resumed.
func (q *queueControl) toggle() {
	q.mu.Lock()
	q.paused = !q.paused
	paused := q.paused
	if paused && q.cancel != nil {
		q.cancelReason = "pause"
		q.cancel()
	}
	q.mu.Unlock()
	q.cond.Broadcast()
	if q.emitPaused != nil {
		q.emitPaused(paused)
	}
}

// remove drops a video from the queue, cancelling it if it is the active one.
func (q *queueControl) remove(path string) {
	q.mu.Lock()
	q.removed[path] = true
	active := q.curPath == path
	if active && q.cancel != nil {
		q.cancelReason = "remove"
		q.cancel()
	}
	q.mu.Unlock()
	q.cond.Broadcast()
	// A pending (not-yet-started) item is dropped from the UI immediately; the
	// active item is reported by the processing loop once its cancel unwinds.
	if !active && q.emitRemoved != nil {
		q.emitRemoved(path)
	}
}

type wailsGUI struct {
	ctx        context.Context
	args       []string
	processing chan struct{}
	startOnce  sync.Once
	controlMu  sync.Mutex
	control    *queueControl
}

func (g *wailsGUI) currentControl() *queueControl {
	g.controlMu.Lock()
	defer g.controlMu.Unlock()
	return g.control
}

func newWailsGUI(args []string) *wailsGUI {
	return &wailsGUI{
		args:       append([]string(nil), args...),
		processing: make(chan struct{}, 1),
	}
}

func (g *wailsGUI) startup(ctx context.Context) {
	g.ctx = ctx
	wailsruntime.OnFileDrop(ctx, func(_ int, _ int, paths []string) {
		g.processDroppedPaths(paths)
	})
	// Wait for the frontend to register its event listeners before kicking
	// off the initial run; otherwise the early status/queue events are emitted
	// to nobody and the queue never appears. The frontend emits this once it
	// has subscribed via window.runtime.EventsOn.
	wailsruntime.EventsOn(ctx, "subtrans:ready", func(...interface{}) {
		g.startOnce.Do(func() {
			g.startRun(func(writer io.Writer, events runEvents) int {
				return runWithEvents(g.args, writer, writer, events)
			})
		})
	})
	wailsruntime.EventsOn(ctx, "subtrans:control:toggle", func(...interface{}) {
		if c := g.currentControl(); c != nil {
			c.toggle()
		}
	})
	wailsruntime.EventsOn(ctx, "subtrans:control:remove", func(data ...interface{}) {
		if len(data) == 0 {
			return
		}
		path, ok := data[0].(string)
		if !ok || path == "" {
			return
		}
		if c := g.currentControl(); c != nil {
			c.remove(path)
		}
	})
}

func (g *wailsGUI) processDroppedPaths(paths []string) {
	paths = filterDroppedPaths(paths)
	if len(paths) == 0 {
		g.emitLog("Drop ignored; no local video file or folder was found.\n")
		return
	}
	opts, err := parseOptions(g.args)
	if err != nil {
		g.emitLog(fmt.Sprintf("Drop ignored; current options are invalid: %v\n", err))
		return
	}
	g.startRun(func(writer io.Writer, events runEvents) int {
		failures := 0
		for _, path := range paths {
			if code := runWithEvents(argsForPath(opts, path), writer, writer, events); code != 0 {
				failures++
			}
		}
		if failures > 0 {
			return 1
		}
		return 0
	})
}

func (g *wailsGUI) startRun(runFunc func(io.Writer, runEvents) int) {
	select {
	case g.processing <- struct{}{}:
	default:
		g.emitLog("Already processing; drop ignored until the current run finishes.\n")
		return
	}

	g.emitStatus("Running...", true, 0)
	writer := wailsLogWriter{emit: g.emitLog}
	control := newQueueControl(g.emitQueueRemove, g.emitPaused)
	g.controlMu.Lock()
	g.control = control
	g.controlMu.Unlock()
	events := runEvents{
		setQueue:     g.emitQueueSet,
		markActive:   g.emitQueueActive,
		removeQueued: g.emitQueueRemove,
		control:      control,
	}
	go func() {
		code := runFunc(writer, events)
		g.controlMu.Lock()
		g.control = nil
		g.controlMu.Unlock()
		if code == 0 {
			g.emitStatus("Completed successfully", false, code)
		} else {
			g.emitStatus(fmt.Sprintf("Completed with errors (exit code %d)", code), false, code)
		}
		<-g.processing
	}()
}

func (g *wailsGUI) emitStatus(message string, running bool, code int) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:status", map[string]any{
		"message": message,
		"running": running,
		"code":    code,
	})
}

func (g *wailsGUI) emitLog(message string) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:log", message)
}

func (g *wailsGUI) emitQueueSet(paths []string) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:queue:set", paths)
}

func (g *wailsGUI) emitPaused(paused bool) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:paused", paused)
}

func (g *wailsGUI) emitQueueActive(path string) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:queue:active", path)
}

func (g *wailsGUI) emitQueueRemove(path string) {
	if g.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(g.ctx, "subtrans:queue:remove", path)
}

type wailsLogWriter struct {
	emit func(string)
}

func (w wailsLogWriter) Write(p []byte) (int, error) {
	w.emit(string(p))
	return len(p), nil
}

func filterDroppedPaths(paths []string) []string {
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() || isVideo(path) {
			filtered = append(filtered, path)
		}
	}
	return filtered
}

func argsForPath(opts options, path string) []string {
	args := []string{
		"-lang", opts.targetLang,
		"-source", opts.sourceLang,
		"-min-size", strconv.FormatInt(opts.minSize, 10),
		"-timeout", opts.timeout.String(),
	}
	if !opts.recursive {
		args = append(args, "-recursive=false")
	}
	if opts.overwrite {
		args = append(args, "-overwrite")
	}
	return append(args, path)
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithEvents(args, stdout, stderr, runEvents{})
}

func runWithEvents(args []string, stdout, stderr io.Writer, events runEvents) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	if opts.register {
		if err := registerExecutableDir(stdout); err != nil {
			fmt.Fprintf(stderr, "register: %v\n", err)
			return 1
		}
		return 0
	}

	logf(stdout, "Starting subtrans %s", version)
	logf(stdout, "Target: path=%q source=%s target=%s recursive=%t overwrite=%t min-size=%d", opts.path, opts.sourceLang, opts.targetLang, opts.recursive, opts.overwrite, opts.minSize)
	logf(stdout, "Scanning for video files...")
	videos, err := findVideos(opts.path, opts.recursive)
	if err != nil {
		fmt.Fprintf(stderr, "find videos: %v\n", err)
		return 1
	}
	if len(videos) == 0 {
		logf(stdout, "No video files found.")
		if events.setQueue != nil {
			events.setQueue(nil)
		}
		return 0
	}
	logf(stdout, "Found %d video file(s).", len(videos))
	if events.setQueue != nil {
		events.setQueue(videos)
	}

	ffmpegPath, err := resolveFFmpeg(stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if events.setQueue != nil {
			events.setQueue(nil)
		}
		return 1
	}

	translator := translateClient{
		httpClient: &http.Client{Timeout: opts.timeout},
		source:     opts.sourceLang,
		target:     opts.targetLang,
	}

	failures := 0
	parentCtx := context.Background()
	for i := 0; i < len(videos); {
		video := videos[i]

		ctx := parentCtx
		if events.control != nil {
			c, proceed := events.control.begin(parentCtx, video)
			if !proceed {
				// Removed while queued; the UI was already updated by remove().
				i++
				continue
			}
			ctx = c
		}

		if events.markActive != nil {
			events.markActive(video)
		}
		logf(stdout, "Processing %d/%d: %s", i+1, len(videos), video)
		err := processVideo(ctx, video, opts, translator, ffmpegPath, stdout)

		reason := ""
		if events.control != nil {
			reason = events.control.done()
		}
		switch reason {
		case "pause":
			// Don't advance: this file is re-processed when the queue resumes.
			logf(stdout, "Paused before finishing %s; will resume this file.", filepath.Base(video))
			continue
		case "remove":
			logf(stdout, "Removed %s from the queue.", filepath.Base(video))
			if events.removeQueued != nil {
				events.removeQueued(video)
			}
			i++
			continue
		}

		if err != nil {
			failures++
			fmt.Fprintf(stderr, "%s: %v\n", video, err)
			logf(stdout, "Failed %d/%d: %s", i+1, len(videos), video)
			if events.removeQueued != nil {
				events.removeQueued(video)
			}
			i++
			continue
		}
		logf(stdout, "Finished %d/%d: %s", i+1, len(videos), video)
		if events.removeQueued != nil {
			events.removeQueued(video)
		}
		i++
	}
	if failures > 0 {
		fmt.Fprintf(stderr, "Completed with %d failure(s).\n", failures)
		return 1
	}
	logf(stdout, "Completed successfully.")
	return 0
}

func parseOptions(args []string) (options, error) {
	fs := flag.NewFlagSet("subtrans", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := options{}
	fs.StringVar(&opts.targetLang, "lang", "he", "target language code")
	fs.StringVar(&opts.sourceLang, "source", "auto", "source language code")
	fs.BoolVar(&opts.recursive, "recursive", true, "search subdirectories")
	fs.BoolVar(&opts.overwrite, "overwrite", false, "overwrite existing translated subtitle files")
	fs.BoolVar(&opts.register, "register", false, "add this executable's directory to the user PATH and exit")
	fs.BoolVar(&opts.cli, "cli", false, "run in command-line mode without opening the graphical log window")
	fs.Int64Var(&opts.minSize, "min-size", 32, "minimum extracted subtitle size in bytes before trying the next subtitle track")
	fs.DurationVar(&opts.timeout, "timeout", 30*time.Second, "HTTP translation timeout")
	showVersion := fs.Bool("version", false, "print version")

	if err := fs.Parse(args); err != nil {
		return opts, usageError()
	}
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	remaining := fs.Args()
	switch len(remaining) {
	case 0:
		opts.path = "."
	case 1:
		opts.path = remaining[0]
	default:
		return opts, usageError()
	}

	if opts.targetLang == "" {
		return opts, errors.New("target language cannot be empty")
	}
	if opts.sourceLang == "" {
		opts.sourceLang = "auto"
	}
	if opts.minSize < 0 {
		return opts, errors.New("min-size cannot be negative")
	}
	return opts, nil
}

func usageError() error {
	return errors.New(`usage: subtrans [flags] [directory-or-video]

Flags:
  -lang string       target language code (default "he")
  -source string     source language code (default "auto")
  -recursive         search subdirectories (default true)
  -overwrite         overwrite existing translated subtitle files
  -register          add this executable's directory to the user PATH and exit
  -cli               run in command-line mode without opening the graphical log window
  -min-size int      minimum usable extracted subtitle size in bytes (default 32)
  -timeout duration  HTTP translation timeout (default 30s)
  -version           print version`)
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "[%s] ", time.Now().Format("15:04:05"))
	fmt.Fprintf(w, format, args...)
	fmt.Fprintln(w)
}

func registerExecutableDir(stdout io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	logf(stdout, "Registering executable directory in PATH: %s", dir)

	if runtime.GOOS == "windows" {
		return registerWindowsPath(dir, stdout)
	}
	return registerShellPath(dir, stdout)
}

func registerShellPath(dir string, stdout io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	profile := shellProfilePath(home)
	block := shellPathBlock(dir)

	content, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, changed := upsertMarkedBlock(string(content), "subtrans register", block)
	if !changed {
		logf(stdout, "PATH registration already present in %s", profile)
		return nil
	}
	if err := os.WriteFile(profile, []byte(updated), 0o644); err != nil {
		return err
	}
	logf(stdout, "Updated %s", profile)
	logf(stdout, "Open a new terminal, or run: source %s", profile)
	return nil
}

func shellProfilePath(home string) string {
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "bash":
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	default:
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".zshrc")
		}
		return filepath.Join(home, ".profile")
	}
}

func shellPathBlock(dir string) string {
	return fmt.Sprintf("export PATH=%s:$PATH\n", shellQuote(dir))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func upsertMarkedBlock(content, name, block string) (string, bool) {
	start := "# >>> " + name + " >>>"
	end := "# <<< " + name + " <<<"
	desired := start + "\n" + block + end + "\n"

	startIndex := strings.Index(content, start)
	endIndex := strings.Index(content, end)
	if startIndex >= 0 && endIndex > startIndex {
		endIndex += len(end)
		if endIndex < len(content) && content[endIndex] == '\n' {
			endIndex++
		}
		if content[startIndex:endIndex] == desired {
			return content, false
		}
		return content[:startIndex] + desired + content[endIndex:], true
	}

	if strings.Contains(content, block) {
		return content, false
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content != "" {
		content += "\n"
	}
	return content + desired, true
}

func registerWindowsPath(dir string, stdout io.Writer) error {
	current, err := currentWindowsUserPath()
	if err != nil {
		return err
	}
	entries := splitWindowsPath(current)
	for _, entry := range entries {
		if strings.EqualFold(filepath.Clean(entry), filepath.Clean(dir)) {
			logf(stdout, "PATH registration already present in the Windows user environment.")
			return nil
		}
	}
	entries = append(entries, dir)
	updated := strings.Join(entries, ";")

	cmd := hiddenCommand("reg", "add", `HKCU\Environment`, "/v", "Path", "/t", "REG_EXPAND_SZ", "/d", updated, "/f")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reg add failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	logf(stdout, "Updated Windows user PATH.")
	logf(stdout, "Open a new terminal for the PATH change to take effect.")
	return nil
}

func currentWindowsUserPath() (string, error) {
	cmd := hiddenCommand("reg", "query", `HKCU\Environment`, "/v", "Path")
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "Path") {
			return strings.Join(fields[2:], " "), nil
		}
	}
	return "", nil
}

func splitWindowsPath(value string) []string {
	raw := strings.Split(value, ";")
	entries := make([]string, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func resolveFFmpeg(stdout io.Writer) (string, error) {
	logf(stdout, "Resolving ffmpeg...")
	if path, ok := bundledFFmpeg(); ok {
		logf(stdout, "Using bundled ffmpeg: %s", path)
		return path, nil
	}
	cachePath, err := ffmpegCachePath()
	if err != nil {
		return "", err
	}
	if executableExists(cachePath) {
		if validFFmpeg(cachePath) {
			logf(stdout, "Using cached ffmpeg: %s", cachePath)
			return cachePath, nil
		}
		logf(stdout, "Cached ffmpeg is not runnable; deleting it and downloading a fresh copy: %s", cachePath)
		_ = os.Remove(cachePath)
	} else {
		logf(stdout, "Cached ffmpeg not found: %s", cachePath)
	}
	sourceURL, err := ffmpegURL()
	if err != nil {
		if path, pathErr := exec.LookPath("ffmpeg"); pathErr == nil {
			logf(stdout, "Automatic ffmpeg download is unavailable for this platform; using ffmpeg from PATH: %s", path)
			return path, nil
		}
		return "", err
	}
	destination := cachePath
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}

	logf(stdout, "No ffmpeg found locally; downloading once for %s/%s.", runtime.GOOS, runtime.GOARCH)
	logf(stdout, "Downloading ffmpeg from %s", sourceURL)
	if err := downloadExecutable(sourceURL, destination); err != nil {
		return "", fmt.Errorf("download ffmpeg: %w", err)
	}
	if !validFFmpeg(destination) {
		_ = os.Remove(destination)
		return "", fmt.Errorf("downloaded ffmpeg is not runnable: %s", destination)
	}
	logf(stdout, "Installed ffmpeg at %s; future runs will reuse this cached copy.", destination)
	return destination, nil
}

func bundledFFmpeg() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	exeDir := filepath.Dir(exe)
	for _, dir := range []string{filepath.Join(exeDir, "ffmpeg"), filepath.Join(exeDir, "bin"), exeDir} {
		path := filepath.Join(dir, executableName("ffmpeg"))
		if executableExists(path) {
			return path, true
		}
	}
	return "", false
}

func executableExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func validFFmpeg(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := hiddenCommandContext(ctx, path, "-version")
	return cmd.Run() == nil
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func ffmpegCachePath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	toolDir := filepath.Join(cacheDir, "subtrans", "ffmpeg", ffmpegStaticVersion, runtime.GOOS+"-"+runtime.GOARCH)
	return filepath.Join(toolDir, executableName("ffmpeg")), nil
}

func ffmpegURL() (string, error) {
	platform, err := ffmpegStaticPlatform()
	if err != nil {
		return "", err
	}
	base := "https://github.com/descriptinc/ffmpeg-ffprobe-static/releases/download/" + ffmpegStaticVersion
	return base + "/ffmpeg-" + platform, nil
}

func ffmpegStaticPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin-arm64", nil
	case "windows/amd64":
		return "win32-x64", nil
	default:
		return "", fmt.Errorf("automatic ffmpeg download is not configured for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func downloadExecutable(sourceURL, destination string) error {
	req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "subtrans/"+version)

	client := http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, sourceURL)
	}

	tmp := destination + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destination)
}

func findVideos(path string, recursive bool) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if isVideo(path) {
			return []string{path}, nil
		}
		return nil, fmt.Errorf("%s is not a supported video file", path)
	}

	var videos []string
	if recursive {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if isVideo(p) {
				videos = append(videos, p)
			}
			return nil
		})
	} else {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			p := filepath.Join(path, entry.Name())
			if isVideo(p) {
				videos = append(videos, p)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(videos)
	return videos, nil
}

func isVideo(path string) bool {
	return videoExtensions[strings.ToLower(filepath.Ext(path))]
}

func processVideo(ctx context.Context, video string, opts options, translator translateClient, ffmpegPath string, stdout io.Writer) error {
	output := subtitleOutputPath(video, opts.targetLang)
	logf(stdout, "Output subtitle path: %s", output)
	if !opts.overwrite {
		if _, err := os.Stat(output); err == nil {
			logf(stdout, "Skipping; output already exists: %s", output)
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	duration := probeDuration(ctx, ffmpegPath, video)
	if duration > 0 {
		logf(stdout, "Video duration: %s.", formatDuration(duration))
	}

	logf(stdout, "Extracting first usable subtitle track...")
	extracted, track, err := extractFirstUsableSubtitle(ctx, video, opts.minSize, ffmpegPath, duration, stdout)
	if err != nil {
		return err
	}
	defer os.Remove(extracted)

	logf(stdout, "Reading extracted subtitle: %s", extracted)
	content, err := os.ReadFile(extracted)
	if err != nil {
		return err
	}
	cues, err := parseSRT(string(content))
	if err != nil {
		return err
	}
	if len(cues) == 0 {
		return errors.New("extracted subtitle contains no cues")
	}
	logf(stdout, "Parsed %d subtitle cue(s).", len(cues))

	logf(stdout, "Translating with subtitle track %d.", track)
	if err := translateCues(ctx, translator, cues, stdout); err != nil {
		return err
	}
	logf(stdout, "Writing translated subtitle...")
	if err := os.WriteFile(output, []byte(renderSRT(cues)), 0o644); err != nil {
		return err
	}
	logf(stdout, "Wrote %s", output)
	return nil
}

func subtitleOutputPath(video, lang string) string {
	ext := filepath.Ext(video)
	base := strings.TrimSuffix(video, ext)
	suffix := lang
	if lang == "he" {
		suffix = "heb"
	}
	return base + "." + suffix + ".srt"
}

func extractFirstUsableSubtitle(ctx context.Context, video string, minSize int64, ffmpegPath string, total time.Duration, stdout io.Writer) (string, int, error) {
	const maxSubtitleTracks = 64
	var lastErr error
	for track := 0; track < maxSubtitleTracks; track++ {
		logf(stdout, "Trying subtitle track %d.", track)
		tmp, err := os.CreateTemp("", "subtrans-*.srt")
		if err != nil {
			return "", 0, err
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()

		cmd := hiddenCommandContext(ctx, ffmpegPath, "-y", "-v", "error", "-progress", "pipe:1", "-i", video, "-map", "0:s:"+strconv.Itoa(track), "-c:s", "srt", tmpPath)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := runWithProgress(cmd, track, total, stdout); err != nil {
			_ = os.Remove(tmpPath)
			if ctx.Err() != nil {
				return "", 0, ctx.Err()
			}
			message := strings.TrimSpace(stderr.String())
			if strings.Contains(message, "matches no streams") || strings.Contains(message, "Stream map") {
				if track == 0 {
					return "", 0, errors.New("no subtitle tracks found")
				}
				logf(stdout, "No subtitle track %d found; stopping subtitle search.", track)
				break
			}
			if strings.Contains(message, "Error opening input") || strings.Contains(message, "Invalid data found when processing input") {
				return "", 0, fmt.Errorf("ffmpeg could not read input: %s", message)
			}
			lastErr = fmt.Errorf("subtitle track %d extraction failed: %w: %s", track, err, message)
			logf(stdout, "Subtitle track %d extraction failed; trying next subtitle track.", track)
			continue
		}

		info, err := os.Stat(tmpPath)
		if err != nil {
			_ = os.Remove(tmpPath)
			lastErr = err
			continue
		}
		if info.Size() <= minSize {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("subtitle track %d extracted subtitle too small (%d bytes)", track, info.Size())
			logf(stdout, "Subtitle track %d extracted %d bytes, below min-size %d; trying next subtitle track.", track, info.Size(), minSize)
			continue
		}
		logf(stdout, "Selected subtitle track %d; extracted %d bytes.", track, info.Size())
		return tmpPath, track, nil
	}
	if lastErr != nil {
		return "", 0, fmt.Errorf("no usable subtitle track found: %w", lastErr)
	}
	return "", 0, errors.New("no usable subtitle track found")
}

// runWithProgress runs an ffmpeg command whose -progress output is piped to
// stdout, emitting a throttled log line so a long extraction shows movement
// instead of sitting silently on "Trying subtitle track N".
func runWithProgress(cmd *exec.Cmd, track int, total time.Duration, stdout io.Writer) error {
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pipe)
		var lastLog time.Time
		for scanner.Scan() {
			value, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "out_time_us=")
			if !ok {
				continue
			}
			us, err := strconv.ParseInt(value, 10, 64)
			if err != nil || us < 0 {
				continue
			}
			now := time.Now()
			if now.Sub(lastLog) < 2*time.Second {
				continue
			}
			lastLog = now
			processed := time.Duration(us) * time.Microsecond
			if total > 0 {
				pct := float64(processed) / float64(total) * 100
				if pct > 100 {
					pct = 100
				}
				logf(stdout, "Subtitle track %d: scanned %s / %s (%.0f%%).", track, formatDuration(processed), formatDuration(total), pct)
			} else {
				logf(stdout, "Subtitle track %d: scanned %s.", track, formatDuration(processed))
			}
		}
	}()

	err = cmd.Wait()
	<-done
	return err
}

// probeDuration reads the container header to determine the total runtime of
// the video. It tolerates failure (returns 0) since duration is only used to
// render a progress percentage.
func probeDuration(ctx context.Context, ffmpegPath, video string) time.Duration {
	cmd := hiddenCommandContext(ctx, ffmpegPath, "-hide_banner", "-i", video)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// ffmpeg exits non-zero here because no output file is specified; the
	// duration we want is printed to stderr before it bails out.
	_ = cmd.Run()
	return parseFFmpegDuration(stderr.String())
}

func parseFFmpegDuration(output string) time.Duration {
	idx := strings.Index(output, "Duration:")
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(output[idx+len("Duration:"):])
	if c := strings.IndexByte(rest, ','); c >= 0 {
		rest = rest[:c]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.HasPrefix(rest, "N/A") {
		return 0
	}
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(rest, "%d:%d:%f", &h, &m, &sec); err != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec*float64(time.Second))
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

func parseSRT(input string) ([]cue, error) {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	blocks := regexp.MustCompile(`\n{2,}`).Split(strings.TrimSpace(normalized), -1)

	cues := make([]cue, 0, len(blocks))
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}

		timingIndex := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				timingIndex = i
				break
			}
		}
		if timingIndex == -1 {
			continue
		}
		if timingIndex == len(lines)-1 {
			return nil, fmt.Errorf("cue has timing but no text: %q", block)
		}
		cues = append(cues, cue{
			Prefix: append([]string(nil), lines[:timingIndex]...),
			Timing: lines[timingIndex],
			Text:   append([]string(nil), lines[timingIndex+1:]...),
		})
	}
	return cues, nil
}

func translateCues(ctx context.Context, translator translateClient, cues []cue, stdout io.Writer) error {
	const maxBatchChars = 3500
	const delimiter = "\n\n<<<SUBTRANS_BREAK>>>\n\n"

	type item struct {
		index int
		text  string
	}
	var batch []item
	batchChars := 0
	batchNumber := 0
	nonEmptyCues := 0
	for i := range cues {
		if strings.TrimSpace(strings.Join(cues[i].Text, "\n")) != "" {
			nonEmptyCues++
		}
	}
	logf(stdout, "Translating %d non-empty cue(s) from %s to %s.", nonEmptyCues, translator.source, translator.target)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		batchNumber++
		firstCue := batch[0].index + 1
		lastCue := batch[len(batch)-1].index + 1
		logf(stdout, "Translating batch %d: cues %d-%d (%d cue(s), %d chars).", batchNumber, firstCue, lastCue, len(batch), batchChars)
		parts := make([]string, len(batch))
		for i, item := range batch {
			parts[i] = item.text
		}
		translated, err := translator.translate(ctx, strings.Join(parts, delimiter))
		if err != nil {
			return err
		}
		translatedParts := strings.Split(translated, strings.TrimSpace(delimiter))
		if len(translatedParts) != len(batch) {
			translatedParts = strings.Split(translated, "<<<SUBTRANS_BREAK>>>")
		}
		if len(translatedParts) != len(batch) {
			logf(stdout, "Batch %d could not be split safely; retrying cue-by-cue.", batchNumber)
			translatedParts = nil
			for _, item := range batch {
				logf(stdout, "Translating cue %d individually.", item.index+1)
				translatedOne, err := translator.translate(ctx, item.text)
				if err != nil {
					return err
				}
				translatedParts = append(translatedParts, translatedOne)
			}
		}
		for i, item := range batch {
			cues[item.index].Text = strings.Split(cleanTranslatedSubtitleText(translatedParts[i]), "\n")
		}
		batch = nil
		batchChars = 0
		logf(stdout, "Finished batch %d.", batchNumber)
		return nil
	}

	for i := range cues {
		text := strings.Join(cues[i].Text, "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		needed := len([]rune(text)) + len([]rune(delimiter))
		if len(batch) > 0 && batchChars+needed > maxBatchChars {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, item{index: i, text: text})
		batchChars += needed
	}
	return flush()
}

func cleanTranslatedSubtitleText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}

func (c translateClient) translate(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}
	endpoint, err := url.Parse("https://translate.googleapis.com/translate_a/single")
	if err != nil {
		return "", err
	}
	q := endpoint.Query()
	q.Set("client", "gtx")
	q.Set("sl", c.source)
	q.Set("tl", c.target)
	q.Set("dt", "t")
	q.Set("q", text)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "subtrans/"+version)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("translate failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload []any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse translate response: %w", err)
	}
	if len(payload) == 0 {
		return "", errors.New("translate response is empty")
	}
	sentences, ok := payload[0].([]any)
	if !ok {
		return "", errors.New("translate response has unexpected shape")
	}
	var builder strings.Builder
	for _, sentence := range sentences {
		parts, ok := sentence.([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		translated, ok := parts[0].(string)
		if ok {
			builder.WriteString(translated)
		}
	}
	result := strings.TrimSpace(builder.String())
	if result == "" {
		return "", errors.New("translate response did not include translated text")
	}
	return result, nil
}

func renderSRT(cues []cue) string {
	var builder strings.Builder
	for i, cue := range cues {
		if i > 0 {
			builder.WriteString("\n\n")
		}
		if len(cue.Prefix) > 0 {
			for _, line := range cue.Prefix {
				builder.WriteString(line)
				builder.WriteByte('\n')
			}
		} else {
			builder.WriteString(strconv.Itoa(i + 1))
			builder.WriteByte('\n')
		}
		builder.WriteString(cue.Timing)
		builder.WriteByte('\n')
		for j, line := range cue.Text {
			if j > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(line)
		}
	}
	builder.WriteByte('\n')
	return builder.String()
}
