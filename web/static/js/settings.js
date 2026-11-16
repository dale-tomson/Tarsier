(function() {
  'use strict';

  function formatBytes(bytes) {
    if (bytes == null || isNaN(bytes) || bytes <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const idx = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
    const val = bytes / Math.pow(1024, idx);
    return (idx === 0 ? val.toFixed(0) : val.toFixed(2)) + ' ' + units[idx];
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

      // Confirmation Modal
      confirmRunModalOpen: false,

      // Custom Template Preview Modal
      templateModalOpen: false,
      templateFormat: 'markdown',
      templateInput: '',
      templatePreview: '',
      templateLoading: false,
      templateError: '',

      formatBytes: formatBytes,

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
      },

      downloadTemplate(format) {
        window.location.href = '/api/v1/system/cleanup/template?format=' + encodeURIComponent(format);
      },

      openTemplateModal() {
        this.templateModalOpen = true;
        this.templateFormat = 'markdown';
        this.templateInput = '';
        this.templatePreview = '';
        this.templateError = '';
      },

      closeTemplateModal() {
        this.templateModalOpen = false;
        this.templatePreview = '';
        this.templateError = '';
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
