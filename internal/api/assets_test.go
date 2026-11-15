package api

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"strings"
	"testing"

	"github.com/dale-tomson/Tarsier/web"
)

func TestAssets_SettingsJSEmbedded(t *testing.T) {
	data, err := web.Assets.ReadFile("static/js/settings.js")
	if err != nil {
		t.Fatalf("expected static/js/settings.js to be embedded, got err: %v", err)
	}
	if !strings.Contains(string(data), "SettingsController") {
		t.Errorf("expected settings.js to define SettingsController")
	}
}

func TestAssets_ZeroVHtml(t *testing.T) {
	err := fs.WalkDir(web.Assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		raw, err := web.Assets.ReadFile(path)
		if err != nil {
			return err
		}

		if strings.Contains(string(raw), "v-html") {
			t.Errorf("prohibited v-html directive found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk assets failed: %v", err)
	}
}

func TestAssets_BundleSizeUnder100KB(t *testing.T) {
	var combined bytes.Buffer

	err := fs.WalkDir(web.Assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		raw, err := web.Assets.ReadFile(path)
		if err != nil {
			return err
		}

		combined.Write(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk assets failed: %v", err)
	}

	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip writer failed: %v", err)
	}
	if _, err := gw.Write(combined.Bytes()); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}

	totalGzipBytes := int64(buf.Len())
	const maxBudgetBytes = 100 * 1024 // 100 KB

	t.Logf("Total embedded gzipped UI assets: %d bytes (%.2f KB) / 100 KB budget", totalGzipBytes, float64(totalGzipBytes)/1024)

	if totalGzipBytes > maxBudgetBytes {
		t.Fatalf("total gzipped UI bundle (%d bytes) exceeds 100 KB budget (%d bytes)", totalGzipBytes, maxBudgetBytes)
	}
}
