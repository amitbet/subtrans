package main

import (
	"strings"
	"testing"
)

func TestParseAndRenderSRT(t *testing.T) {
	input := "1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\nworld\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\nBye\r\n"

	cues, err := parseSRT(input)
	if err != nil {
		t.Fatalf("parseSRT() error = %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("len(cues) = %d, want 2", len(cues))
	}
	if got := strings.Join(cues[0].Text, "\n"); got != "Hello\nworld" {
		t.Fatalf("first cue text = %q", got)
	}

	cues[0].Text = []string{"שלום", "עולם"}
	rendered := renderSRT(cues)
	if !strings.Contains(rendered, "1\n00:00:01,000 --> 00:00:02,000\nשלום\nעולם") {
		t.Fatalf("rendered SRT missing translated first cue:\n%s", rendered)
	}
}

func TestSubtitleOutputPath(t *testing.T) {
	tests := []struct {
		video string
		lang  string
		want  string
	}{
		{"movie.mkv", "he", "movie.heb.srt"},
		{"movie.mkv", "fr", "movie.fr.srt"},
		{"/tmp/show/movie.name.mp4", "he", "/tmp/show/movie.name.heb.srt"},
	}

	for _, tt := range tests {
		if got := subtitleOutputPath(tt.video, tt.lang); got != tt.want {
			t.Fatalf("subtitleOutputPath(%q, %q) = %q, want %q", tt.video, tt.lang, got, tt.want)
		}
	}
}

func TestUpsertMarkedBlock(t *testing.T) {
	content := "existing=true\n"
	block := "export PATH='/opt/subtrans':$PATH\n"

	updated, changed := upsertMarkedBlock(content, "subtrans register", block)
	if !changed {
		t.Fatal("upsertMarkedBlock() changed = false, want true")
	}
	if !strings.Contains(updated, "# >>> subtrans register >>>\n"+block+"# <<< subtrans register <<<") {
		t.Fatalf("updated content missing marked block:\n%s", updated)
	}

	again, changed := upsertMarkedBlock(updated, "subtrans register", block)
	if changed {
		t.Fatal("second upsertMarkedBlock() changed = true, want false")
	}
	if again != updated {
		t.Fatal("second upsertMarkedBlock() changed content unexpectedly")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/tmp/has space/it's"); got != "'/tmp/has space/it'\"'\"'s'" {
		t.Fatalf("shellQuote() = %q", got)
	}
}
