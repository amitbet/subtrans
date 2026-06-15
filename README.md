# subtrans

`subtrans` extracts the first usable subtitle track from video files, translates it, and writes a sidecar subtitle that VLC can auto-load.

By default it searches the current directory recursively, translates to Hebrew, and writes files like:

```text
Movie.mkv
Movie.heb.srt
```

## Dependencies

- Internet access for translation

Release downloads contain only the `subtrans` executable. On first use, `subtrans` looks for a cached `ffmpeg`; if it is missing, it downloads `ffmpeg` into the OS user cache and reuses that cached copy on later runs. If the cache is deleted or the cached binary stops working, `subtrans` downloads a fresh copy. It does not place `ffmpeg` next to the executable.

The translator uses the free, unofficial Google Translate endpoint. It does not require an API key, but it is not an SLA-backed Google Cloud API and may rate-limit or change behavior. Translation still requires network access.

## Usage

```sh
subtrans [flags] [directory-or-video]
```

Examples:

```sh
subtrans
subtrans /path/to/videos
subtrans -lang fr -overwrite movie.mkv
subtrans -recursive=false ~/Movies
subtrans --register
```

The command prints timestamped progress logs while it scans, resolves cached `ffmpeg`, extracts subtitles, translates batches, and writes output files.

Use `subtrans --register` once to add the executable's directory to your user PATH so future terminals can run `subtrans` from any directory.

Flags:

```text
  -lang string       target language code (default "he")
  -source string     source language code (default "auto")
  -recursive         search subdirectories (default true)
  -overwrite         overwrite existing translated subtitle files
  -register          add this executable's directory to the user PATH and exit
  -min-size int      minimum usable extracted subtitle size in bytes (default 32)
  -timeout duration  HTTP translation timeout (default 30s)
  -version           print version
```

## Release builds

The GitHub Actions workflow builds:

- Windows amd64: `subtrans-windows-amd64.exe`
- macOS arm64: `subtrans-darwin-arm64`

On a push to `main` or a manual workflow run, the workflow creates the next patch tag. If the repository has no tags yet, it starts at `v0.1.0`, then creates a GitHub release with downloadable executable assets and a commit history in the release description.
