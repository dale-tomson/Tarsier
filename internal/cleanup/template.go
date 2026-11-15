package cleanup

import (
	"bytes"
	"fmt"
	htmltmpl "html/template"
	"os"
	texttmpl "text/template"
	"unicode/utf8"
)

// DefaultMarkdownTemplate is the built-in Go text/template used for Discord and plain-text digests.
const DefaultMarkdownTemplate = `# Tarsier Pre-Cleanup Report
**Generated:** {{ .GeneratedAt.Format "2006-01-02 15:04:05 UTC" }}
**Mode:** {{ if .DryRun }}DRY RUN (Preview Only){{ else }}LIVE EXECUTION{{ end }}
**Window:** {{ .WindowStart.Format "2006-01-02 15:04:05" }} - {{ .WindowEnd.Format "2006-01-02 15:04:05 UTC" }}

## Summary
- **Logs Purged:** {{ .TotalLogsPurged }} lines ({{ .TotalErrors }} errors, {{ .TotalWarnings }} warnings)
- **Disk Reclaimed:** {{ formatBytes .TotalBytesReclaim }}
- **Alerts Fired:** {{ .AlertsFired }}

## Source Breakdown
| Source ID | Source Name | Files | Total Lines | Errors | Warnings | Reclaimed |
|-----------|-------------|-------|-------------|--------|----------|-----------|
{{- range .Sources }}
| {{ .SourceID }} | {{ .SourceName }} | {{ .FilesCount }} | {{ .TotalLines }} | {{ .ErrorLines }} | {{ .WarnLines }} | {{ formatBytes .BytesReclaim }} |
{{- end }}

## Top Error Patterns
{{- if .TopErrorPatterns }}
{{- range $i, $p := .TopErrorPatterns }}
### {{ add $i 1 }}. ({{ $p.Count }}x) [{{ $p.SourceName }}]
` + "`" + `{{ $p.Pattern }}` + "`" + `
> Sample: ` + "`" + `{{ $p.SampleLine }}` + "`" + `
{{- end }}
{{- else }}
*No recurring error patterns detected.*
{{- end }}

## Container Activity
{{- if .ContainerActivity }}
| Container | Restarts | OOM Kills | Dies |
|-----------|----------|-----------|------|
{{- range .ContainerActivity }}
| {{ .ContainerName }} | {{ .Restarts }} | {{ .OOMKills }} | {{ .Dies }} |
{{- end }}
{{- else }}
*No container lifecycle events recorded.*
{{- end }}

## Metric Summaries
{{- if .MetricSummaries }}
| Entity | Metric | Min | Avg | Max | Samples |
|--------|--------|-----|-----|-----|---------|
{{- range .MetricSummaries }}
| {{ .Entity }} | {{ .Metric }} | {{ printf "%.2f" .Min }} | {{ printf "%.2f" .Avg }} | {{ printf "%.2f" .Max }} | {{ .Count }} |
{{- end }}
{{- else }}
*No expiring metric samples.*
{{- end }}
`

// DefaultHTMLTemplate is the built-in HTML template (rendered via html/template for XSS protection).
const DefaultHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Tarsier Pre-Cleanup Report</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; margin: 20px; color: #24292f; background: #fff; line-height: 1.5; }
    h1 { font-size: 20px; border-bottom: 2px solid #eaecef; padding-bottom: 8px; margin-bottom: 16px; }
    h2 { font-size: 16px; margin-top: 24px; margin-bottom: 8px; border-bottom: 1px solid #eaecef; padding-bottom: 4px; }
    .badge { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 12px; font-weight: bold; }
    .badge-dry { background: #fff8c5; color: #9a6700; border: 1px solid #d4a72c; }
    .badge-live { background: #dafbe1; color: #1a7f37; border: 1px solid #4ac26b; }
    .stats-card { background: #f6f8fa; border: 1px solid #d0d7de; border-radius: 6px; padding: 12px; margin-bottom: 16px; }
    table { width: 100%; border-collapse: collapse; margin-top: 8px; font-size: 13px; }
    th, td { border: 1px solid #d0d7de; padding: 6px 10px; text-align: left; }
    th { background: #f6f8fa; }
    code, pre { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; font-size: 12px; background: #f6f8fa; border-radius: 3px; padding: 2px 4px; }
    .pattern-card { background: #f6f8fa; border: 1px solid #d0d7de; border-radius: 6px; padding: 8px 12px; margin-bottom: 8px; }
  </style>
</head>
<body>
  <h1>Tarsier Pre-Cleanup Report</h1>
  <div class="stats-card">
    <div><strong>Generated:</strong> {{ .GeneratedAt.Format "2006-01-02 15:04:05 UTC" }}</div>
    <div><strong>Mode:</strong> {{ if .DryRun }}<span class="badge badge-dry">DRY RUN (Preview)</span>{{ else }}<span class="badge badge-live">LIVE EXECUTION</span>{{ end }}</div>
    <div><strong>Window:</strong> {{ .WindowStart.Format "2006-01-02 15:04:05" }} - {{ .WindowEnd.Format "2006-01-02 15:04:05 UTC" }}</div>
    <div><strong>Purged Logs:</strong> {{ .TotalLogsPurged }} lines ({{ .TotalErrors }} errors, {{ .TotalWarnings }} warnings)</div>
    <div><strong>Reclaimed Space:</strong> {{ formatBytes .TotalBytesReclaim }}</div>
    <div><strong>Alerts Fired:</strong> {{ .AlertsFired }}</div>
  </div>

  <h2>Source Breakdown</h2>
  <table>
    <thead>
      <tr>
        <th>Source ID</th>
        <th>Source Name</th>
        <th>Files</th>
        <th>Total Lines</th>
        <th>Errors</th>
        <th>Warnings</th>
        <th>Reclaimed</th>
      </tr>
    </thead>
    <tbody>
      {{- range .Sources }}
      <tr>
        <td>{{ .SourceID }}</td>
        <td>{{ .SourceName }}</td>
        <td>{{ .FilesCount }}</td>
        <td>{{ .TotalLines }}</td>
        <td>{{ .ErrorLines }}</td>
        <td>{{ .WarnLines }}</td>
        <td>{{ formatBytes .BytesReclaim }}</td>
      </tr>
      {{- end }}
    </tbody>
  </table>

  <h2>Top Error Patterns</h2>
  {{- if .TopErrorPatterns }}
    {{- range $i, $p := .TopErrorPatterns }}
    <div class="pattern-card">
      <div><strong>#{{ add $i 1 }} ({{ $p.Count }}x) [{{ $p.SourceName }}]</strong></div>
      <div style="margin: 4px 0;"><code>{{ $p.Pattern }}</code></div>
      <div><small>Sample: <code>{{ $p.SampleLine }}</code></small></div>
    </div>
    {{- end }}
  {{- else }}
    <p><em>No recurring error patterns detected.</em></p>
  {{- end }}

  <h2>Container Activity</h2>
  {{- if .ContainerActivity }}
  <table>
    <thead>
      <tr>
        <th>Container</th>
        <th>Restarts</th>
        <th>OOM Kills</th>
        <th>Dies</th>
      </tr>
    </thead>
    <tbody>
      {{- range .ContainerActivity }}
      <tr>
        <td>{{ .ContainerName }}</td>
        <td>{{ .Restarts }}</td>
        <td>{{ .OOMKills }}</td>
        <td>{{ .Dies }}</td>
      </tr>
      {{- end }}
    </tbody>
  </table>
  {{- else }}
    <p><em>No container lifecycle events recorded.</em></p>
  {{- end }}

  <h2>Metric Summaries</h2>
  {{- if .MetricSummaries }}
  <table>
    <thead>
      <tr>
        <th>Entity</th>
        <th>Metric</th>
        <th>Min</th>
        <th>Avg</th>
        <th>Max</th>
        <th>Samples</th>
      </tr>
    </thead>
    <tbody>
      {{- range .MetricSummaries }}
      <tr>
        <td>{{ .Entity }}</td>
        <td>{{ .Metric }}</td>
        <td>{{ printf "%.2f" .Min }}</td>
        <td>{{ printf "%.2f" .Avg }}</td>
        <td>{{ printf "%.2f" .Max }}</td>
        <td>{{ .Count }}</td>
      </tr>
      {{- end }}
    </tbody>
  </table>
  {{- else }}
    <p><em>No expiring metric samples.</em></p>
  {{- end }}
</body>
</html>
`

// TemplateEngine manages parsing and rendering of Markdown and HTML pre-cleanup reports.
type TemplateEngine struct {
	mdTemplate   *texttmpl.Template
	htmlTemplate *htmltmpl.Template
}

// FormatBytes formats a byte count into a human-readable unit string.
func FormatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	const unit = 1024
	if bytes < unit*1024 {
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(unit))
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"MiB", "GiB", "TiB"}
	return fmt.Sprintf("%.2f %s", float64(bytes)/float64(div), units[exp])
}

func templateFuncMap() texttmpl.FuncMap {
	return texttmpl.FuncMap{
		"formatBytes": FormatBytes,
		"add": func(a, b int) int {
			return a + b
		},
	}
}

func htmlFuncMap() htmltmpl.FuncMap {
	return htmltmpl.FuncMap{
		"formatBytes": FormatBytes,
		"add": func(a, b int) int {
			return a + b
		},
	}
}

// NewTemplateEngine creates an engine loading custom templates if provided, or defaults.
func NewTemplateEngine(mdPath, htmlPath string) (*TemplateEngine, error) {
	mdStr := DefaultMarkdownTemplate
	if mdPath != "" {
		if content, err := os.ReadFile(mdPath); err == nil {
			mdStr = string(content)
		}
	}

	htmlStr := DefaultHTMLTemplate
	if htmlPath != "" {
		if content, err := os.ReadFile(htmlPath); err == nil {
			htmlStr = string(content)
		}
	}

	mdTmpl, err := texttmpl.New("report_md").Funcs(templateFuncMap()).Parse(mdStr)
	if err != nil {
		return nil, fmt.Errorf("parse markdown template: %w", err)
	}

	htmlTmpl, err := htmltmpl.New("report_html").Funcs(htmlFuncMap()).Parse(htmlStr)
	if err != nil {
		return nil, fmt.Errorf("parse html template: %w", err)
	}

	return &TemplateEngine{
		mdTemplate:   mdTmpl,
		htmlTemplate: htmlTmpl,
	}, nil
}

// RenderMarkdown renders the report to Markdown and enforces maxBytes cap with clean UTF-8 truncation.
func (e *TemplateEngine) RenderMarkdown(report PreCleanupReport, maxBytes int) ([]byte, bool, error) {
	var buf bytes.Buffer
	if err := e.mdTemplate.Execute(&buf, report); err != nil {
		return nil, false, err
	}

	raw := buf.Bytes()
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return raw, false, nil
	}

	// Truncate cleanly at a valid UTF-8 boundary
	notice := fmt.Sprintf("\n\n[Report truncated at %d bytes — see Tarsier UI for full details]", maxBytes)
	noticeBytes := []byte(notice)
	allowedLen := maxBytes - len(noticeBytes)
	if allowedLen <= 0 {
		return raw[:maxBytes], true, nil
	}

	// Backtrack to valid UTF-8 boundary
	for allowedLen > 0 && !utf8.RuneStart(raw[allowedLen]) {
		allowedLen--
	}

	truncated := make([]byte, allowedLen+len(noticeBytes))
	copy(truncated, raw[:allowedLen])
	copy(truncated[allowedLen:], noticeBytes)

	return truncated, true, nil
}

// RenderHTML renders the report to HTML and enforces maxBytes cap with clean UTF-8 truncation.
func (e *TemplateEngine) RenderHTML(report PreCleanupReport, maxBytes int) ([]byte, bool, error) {
	var buf bytes.Buffer
	if err := e.htmlTemplate.Execute(&buf, report); err != nil {
		return nil, false, err
	}

	raw := buf.Bytes()
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return raw, false, nil
	}

	notice := fmt.Sprintf("\n<!-- Report truncated at %d bytes -->\n<p><em>[Report truncated at %d bytes — see Tarsier UI for full details]</em></p></body></html>", maxBytes, maxBytes)
	noticeBytes := []byte(notice)
	allowedLen := maxBytes - len(noticeBytes)
	if allowedLen <= 0 {
		return raw[:maxBytes], true, nil
	}

	for allowedLen > 0 && !utf8.RuneStart(raw[allowedLen]) {
		allowedLen--
	}

	truncated := make([]byte, allowedLen+len(noticeBytes))
	copy(truncated, raw[:allowedLen])
	copy(truncated[allowedLen:], noticeBytes)

	return truncated, true, nil
}
