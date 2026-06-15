package main

import (
	"bytes"
	"context"
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
	"time"
)

var version = "dev"

const ffmpegStaticVersion = "b6.1.2-rc.1"

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
	minSize    int64
	timeout    time.Duration
	path       string
}

type mediaTools struct {
	ffmpeg  string
	ffprobe string
}

type probeResult struct {
	Streams []probeStream `json:"streams"`
}

type probeStream struct {
	Index     int               `json:"index"`
	CodecType string            `json:"codec_type"`
	CodecName string            `json:"codec_name"`
	Tags      map[string]string `json:"tags"`
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
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	videos, err := findVideos(opts.path, opts.recursive)
	if err != nil {
		fmt.Fprintf(stderr, "find videos: %v\n", err)
		return 1
	}
	if len(videos) == 0 {
		fmt.Fprintln(stdout, "No video files found.")
		return 0
	}

	tools, err := resolveMediaTools(stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	translator := translateClient{
		httpClient: &http.Client{Timeout: opts.timeout},
		source:     opts.sourceLang,
		target:     opts.targetLang,
	}

	failures := 0
	for _, video := range videos {
		if err := processVideo(video, opts, translator, tools, stdout); err != nil {
			failures++
			fmt.Fprintf(stderr, "%s: %v\n", video, err)
		}
	}
	if failures > 0 {
		fmt.Fprintf(stderr, "Completed with %d failure(s).\n", failures)
		return 1
	}
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
  -min-size int      minimum usable extracted subtitle size in bytes (default 32)
  -timeout duration  HTTP translation timeout (default 30s)
  -version           print version`)
}

func resolveMediaTools(stdout io.Writer) (mediaTools, error) {
	if tools, ok := bundledMediaTools(); ok {
		return tools, nil
	}
	if tools, ok := cachedMediaTools(); ok {
		return tools, nil
	}
	if tools, err := pathMediaTools(); err == nil {
		return tools, nil
	}

	urls, err := mediaToolURLs()
	if err != nil {
		return mediaTools{}, err
	}
	tools, err := mediaToolCachePaths()
	if err != nil {
		return mediaTools{}, err
	}
	if err := os.MkdirAll(filepath.Dir(tools.ffmpeg), 0o755); err != nil {
		return mediaTools{}, err
	}

	fmt.Fprintf(stdout, "Downloading ffmpeg tools for %s/%s...\n", runtime.GOOS, runtime.GOARCH)
	if err := downloadExecutable(urls.ffmpeg, tools.ffmpeg); err != nil {
		return mediaTools{}, fmt.Errorf("download ffmpeg: %w", err)
	}
	if err := downloadExecutable(urls.ffprobe, tools.ffprobe); err != nil {
		return mediaTools{}, fmt.Errorf("download ffprobe: %w", err)
	}
	return tools, nil
}

func bundledMediaTools() (mediaTools, bool) {
	exe, err := os.Executable()
	if err != nil {
		return mediaTools{}, false
	}
	exeDir := filepath.Dir(exe)
	for _, dir := range []string{filepath.Join(exeDir, "ffmpeg"), filepath.Join(exeDir, "bin"), exeDir} {
		tools := mediaTools{
			ffmpeg:  filepath.Join(dir, executableName("ffmpeg")),
			ffprobe: filepath.Join(dir, executableName("ffprobe")),
		}
		if tools.exist() {
			return tools, true
		}
	}
	return mediaTools{}, false
}

func cachedMediaTools() (mediaTools, bool) {
	tools, err := mediaToolCachePaths()
	if err != nil {
		return mediaTools{}, false
	}
	return tools, tools.exist()
}

func pathMediaTools() (mediaTools, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return mediaTools{}, err
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return mediaTools{}, err
	}
	return mediaTools{ffmpeg: ffmpegPath, ffprobe: ffprobePath}, nil
}

func (t mediaTools) exist() bool {
	return executableExists(t.ffmpeg) && executableExists(t.ffprobe)
}

func executableExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func mediaToolCachePaths() (mediaTools, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return mediaTools{}, err
	}
	toolDir := filepath.Join(cacheDir, "subtrans", "ffmpeg", ffmpegStaticVersion, runtime.GOOS+"-"+runtime.GOARCH)
	return mediaTools{
		ffmpeg:  filepath.Join(toolDir, executableName("ffmpeg")),
		ffprobe: filepath.Join(toolDir, executableName("ffprobe")),
	}, nil
}

func mediaToolURLs() (mediaTools, error) {
	platform, err := ffmpegStaticPlatform()
	if err != nil {
		return mediaTools{}, err
	}
	base := "https://github.com/descriptinc/ffmpeg-ffprobe-static/releases/download/" + ffmpegStaticVersion
	return mediaTools{
		ffmpeg:  base + "/ffmpeg-" + platform,
		ffprobe: base + "/ffprobe-" + platform,
	}, nil
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

func processVideo(video string, opts options, translator translateClient, tools mediaTools, stdout io.Writer) error {
	output := subtitleOutputPath(video, opts.targetLang)
	if !opts.overwrite {
		if _, err := os.Stat(output); err == nil {
			fmt.Fprintf(stdout, "Skipping %s, %s already exists.\n", video, output)
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	streams, err := subtitleStreams(video, tools)
	if err != nil {
		return err
	}
	if len(streams) == 0 {
		return errors.New("no subtitle tracks found")
	}

	extracted, stream, err := extractFirstUsableSubtitle(video, streams, opts.minSize, tools)
	if err != nil {
		return err
	}
	defer os.Remove(extracted)

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

	fmt.Fprintf(stdout, "Translating %s using subtitle stream #%d", video, stream.Index)
	if lang := stream.Tags["language"]; lang != "" {
		fmt.Fprintf(stdout, " (%s)", lang)
	}
	fmt.Fprintln(stdout, "...")

	if err := translateCues(context.Background(), translator, cues); err != nil {
		return err
	}
	if err := os.WriteFile(output, []byte(renderSRT(cues)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s\n", output)
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

func subtitleStreams(video string, tools mediaTools) ([]probeStream, error) {
	cmd := exec.Command(tools.ffprobe, "-v", "error", "-print_format", "json", "-show_streams", "-select_streams", "s", video)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
	}
	var result probeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	return result.Streams, nil
}

func extractFirstUsableSubtitle(video string, streams []probeStream, minSize int64, tools mediaTools) (string, probeStream, error) {
	var lastErr error
	for _, stream := range streams {
		tmp, err := os.CreateTemp("", "subtrans-*.srt")
		if err != nil {
			return "", probeStream{}, err
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()

		cmd := exec.Command(tools.ffmpeg, "-y", "-v", "error", "-i", video, "-map", "0:"+strconv.Itoa(stream.Index), "-c:s", "srt", tmpPath)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			_ = os.Remove(tmpPath)
			lastErr = fmt.Errorf("stream #%d extraction failed: %w: %s", stream.Index, err, strings.TrimSpace(stderr.String()))
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
			lastErr = fmt.Errorf("stream #%d extracted subtitle too small (%d bytes)", stream.Index, info.Size())
			continue
		}
		return tmpPath, stream, nil
	}
	if lastErr != nil {
		return "", probeStream{}, fmt.Errorf("no usable subtitle track found: %w", lastErr)
	}
	return "", probeStream{}, errors.New("no usable subtitle track found")
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

func translateCues(ctx context.Context, translator translateClient, cues []cue) error {
	const maxBatchChars = 3500
	const delimiter = "\n\n<<<SUBTRANS_BREAK>>>\n\n"

	type item struct {
		index int
		text  string
	}
	var batch []item
	batchChars := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
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
			translatedParts = nil
			for _, item := range batch {
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
