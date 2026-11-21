(function() {
  'use strict';

  function formatBytes(bytes) {
    if (bytes == null || isNaN(bytes) || bytes <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const idx = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
    const val = bytes / Math.pow(1024, idx);
    return (idx === 0 ? val.toFixed(0) : val.toFixed(2)) + ' ' + units[idx];
  }

  function formatDuration(ms) {
    if (ms == null || isNaN(ms) || ms < 0) return '0ms';
    if (ms < 1000) return ms + 'ms';
    const totalSecs = ms / 1000;
    if (totalSecs < 60) {
      return totalSecs.toFixed(totalSecs < 10 ? 1 : 0) + 's';
    }
    const mins = Math.floor(totalSecs / 60);
    const secs = Math.round(totalSecs % 60);
    if (mins < 60) {
      return secs > 0 ? (mins + 'm ' + secs + 's') : (mins + 'm');
    }
    const hrs = Math.floor(mins / 60);
    const remMins = mins % 60;
    return remMins > 0 ? (hrs + 'h ' + remMins + 'm') : (hrs + 'h');
  }

  function escapeHTML(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function wrapInReportDocument(bodyHTML) {
    return '<!DOCTYPE html>\n<html>\n<head>\n<meta charset="utf-8">\n<style>\n' +
      'body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; padding: 16px; color: #24292f; background: #fff; line-height: 1.5; font-size: 13px; margin: 0; }\n' +
      'h1 { font-size: 18px; border-bottom: 2px solid #eaecef; padding-bottom: 6px; margin: 0 0 12px 0; color: #1f2328; }\n' +
      'h2 { font-size: 14px; border-bottom: 1px solid #eaecef; padding-bottom: 4px; margin: 18px 0 8px 0; color: #1f2328; }\n' +
      'h3 { font-size: 13px; margin: 12px 0 4px 0; color: #1f2328; }\n' +
      'h4 { font-size: 12px; margin: 8px 0 4px 0; color: #1f2328; }\n' +
      'table { width: 100%; border-collapse: collapse; margin: 8px 0 16px 0; font-size: 12px; }\n' +
      'th, td { border: 1px solid #d0d7de; padding: 6px 10px; text-align: left; }\n' +
      'th { background: #f6f8fa; font-weight: 600; color: #24292f; }\n' +
      'tr:nth-child(even) { background: #fbfcfd; }\n' +
      'code { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; font-size: 12px; background: #f6f8fa; border: 1px solid #e1e4e8; border-radius: 3px; padding: 2px 4px; }\n' +
      'blockquote { border-left: 3px solid #d0d7de; color: #57606a; margin: 4px 0 8px 0; padding: 0 8px; }\n' +
      'ul { margin: 4px 0 12px 20px; padding: 0; }\n' +
      'li { margin-bottom: 4px; }\n' +
      'p { margin: 4px 0 8px 0; }\n' +
      'strong { font-weight: 600; }\n' +
      '</style>\n</head>\n<body>\n' + bodyHTML + '\n</body>\n</html>';
  }

  function SettingsController(store) {
    return {
      history: [],
      historyLoading: false,
      dryRunLoading: false,
      manualRunLoading: false,
      reloadLoading: false,

      // Stored Report Modal
      reportModalOpen: false,
      reportLoading: false,
      selectedReport: null,
      reportViewMode: 'rendered', // 'rendered' | 'raw'

      // Confirmation Modal
      confirmRunModalOpen: false,

      // Custom Template Preview Modal
      templateModalOpen: false,
      templateFormat: 'text',
      templateInput: '',
      templatePreview: '',
      templateLoading: false,
      templateError: '',
      templatePreviewMode: 'rendered', // 'rendered' | 'raw'

      formatBytes: formatBytes,
      formatDuration: formatDuration,

      getReportSrcdoc() {
        if (!this.selectedReport || !this.selectedReport.report_body) return '';
        const body = this.selectedReport.report_body.trim();
        if (body.startsWith('<!DOCTYPE') || body.startsWith('<html') || this.selectedReport.report_format === 'html') {
          return body;
        }
        return wrapInReportDocument('<pre style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; white-space:pre-wrap; margin:0; line-height:1.4;">' + escapeHTML(body) + '</pre>');
      },

      toggleReportViewMode() {
        this.reportViewMode = (this.reportViewMode === 'raw' ? 'rendered' : 'raw');
      },

      getTemplatePreviewSrcdoc() {
        if (!this.templatePreview) return '';
        const body = this.templatePreview.trim();
        if (body.startsWith('<!DOCTYPE') || body.startsWith('<html') || this.templateFormat === 'html') {
          return body;
        }
        return wrapInReportDocument('<pre style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; white-space:pre-wrap; margin:0; line-height:1.4;">' + escapeHTML(body) + '</pre>');
      },

      toggleTemplatePreviewMode() {
        this.templatePreviewMode = (this.templatePreviewMode === 'raw' ? 'rendered' : 'raw');
      },

      mounted() {
        this.fetchHistory();
      },

      async fetchHistory() {
        this.historyLoading = true;
        try {
          const data = await store.apiFetch('/api/v1/system/cleanup/history');
          this.history = Array.isArray(data) ? data : [];
        } catch (e) {
          this.history = [];
        } finally {
          this.historyLoading = false;
        }
      },

      async triggerDryRun() {
        this.dryRunLoading = true;
        try {
          const res = await store.apiFetch('/api/v1/system/cleanup/run?dry_run=true', {
            method: 'POST'
          });
          store.toast.show(
            'Dry-run completed: would reclaim ' + formatBytes(res.bytes_reclaimed || 0) +
            ' across ' + (res.logs_deleted || 0) + ' log lines',
            'info',
            5000
          );
          await this.fetchHistory();
          await store.loadSettings();
          if (res.id) {
            this.viewReport(res.id);
          }
        } catch (err) {
          // Toast is shown by store.apiFetch
        } finally {
          this.dryRunLoading = false;
        }
      },

      openConfirmRun() {
        this.confirmRunModalOpen = true;
      },

      closeConfirmRun() {
        this.confirmRunModalOpen = false;
      },

      async triggerCleanup() {
        this.closeConfirmRun();
        this.manualRunLoading = true;
        store.toast.show('Retention cleanup started...', 'info', 3000);
        try {
          const res = await store.apiFetch('/api/v1/system/cleanup/run?dry_run=false', {
            method: 'POST'
          });
          if (res && res.status === 'blocked') {
            store.toast.show(
              'Cleanup blocked by delivery policy: ' + (res.report_detail || 'report delivery failed'),
              'warning',
              7000
            );
          } else {
            store.toast.show(
              'Cleanup completed: reclaimed ' + formatBytes((res && res.bytes_reclaimed) || 0) +
              ' (' + ((res && res.logs_deleted) || 0) + ' logs, ' + ((res && res.metrics_deleted) || 0) + ' metric rows)',
              'success',
              5000
            );
          }
          await this.fetchHistory();
          await store.loadSettings();
        } catch (err) {
          // Toast is shown by store.apiFetch
        } finally {
          this.manualRunLoading = false;
        }
      },

      async reloadConfig() {
        this.reloadLoading = true;
        try {
          await store.apiFetch('/api/v1/system/reload', { method: 'POST' });
          store.toast.show('Configuration reloaded successfully', 'success', 3000);
          await store.loadSettings();
        } catch (err) {
          // Toast is shown by store.apiFetch
        } finally {
          this.reloadLoading = false;
        }
      },

      async viewReport(id) {
        this.reportLoading = true;
        this.reportModalOpen = true;
        this.selectedReport = null;
        this.reportViewMode = 'rendered';
        try {
          const data = await store.apiFetch('/api/v1/system/cleanup/' + id + '/report');
          this.selectedReport = data;
        } catch (err) {
          this.closeReportModal();
        } finally {
          this.reportLoading = false;
        }
      },

      closeReportModal() {
        this.reportModalOpen = false;
        this.selectedReport = null;
        this.reportViewMode = 'rendered';
      },

      downloadTemplate(format) {
        window.location.href = '/api/v1/system/cleanup/template?format=' + encodeURIComponent(format);
      },

      openTemplateModal() {
        this.templateModalOpen = true;
        this.templateFormat = 'text';
        this.templateInput = '';
        this.templatePreview = '';
        this.templateError = '';
        this.templatePreviewMode = 'rendered';
      },

      closeTemplateModal() {
        this.templateModalOpen = false;
        this.templatePreview = '';
        this.templateError = '';
        this.templatePreviewMode = 'rendered';
      },

      async previewTemplate() {
        if (!this.templateInput.trim()) {
          this.templateError = 'Template content cannot be empty';
          return;
        }
        this.templateLoading = true;
        this.templateError = '';
        try {
          const res = await store.apiFetch('/api/v1/system/cleanup/preview', {
            method: 'POST',
            body: {
              format: this.templateFormat,
              template: this.templateInput
            }
          });
          this.templatePreview = res.rendered || '';
        } catch (err) {
          this.templateError = err.message || 'Template preview failed';
          this.templatePreview = '';
        } finally {
          this.templateLoading = false;
        }
      }
    };
  }

  window.SettingsController = SettingsController;
})();
