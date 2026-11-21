(function() {
  'use strict';

  const DRY_RUN_TIMEOUT_MS = 60000;

  function fmtTime(v) {
    if (!v) return '--';
    try {
      const d = typeof v === 'number' ? new Date(v * 1000) : new Date(v);
      if (isNaN(d.getTime())) return String(v);
      const p = n => String(n).padStart(2, '0');
      return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`;
    } catch (_) {
      return String(v);
    }
  }

  function fmtDur(s) {
    if (s == null || isNaN(s) || s <= 0) return 'Instant';
    if (s < 60) return s + 's';
    if (s < 3600) return `${Math.floor(s / 60)}m` + (s % 60 ? ` ${s % 60}s` : '');
    return `${Math.floor(s / 3600)}h` + (Math.floor((s % 3600) / 60) ? ` ${Math.floor((s % 3600) / 60)}m` : '');
  }

  const METRIC_OPTIONS = [
    { id: 'c-cpu', category: 'Container', label: 'Container CPU Usage (%)', value: 'cpu_pct', unit: '%' },
    { id: 'c-mem', category: 'Container', label: 'Container Memory Usage (%)', value: 'mem_pct', unit: '%' },
    { id: 'c-mem-used', category: 'Container', label: 'Container Memory Used (Bytes)', value: 'mem_used', unit: 'Bytes' },
    { id: 'c-net-in', category: 'Container', label: 'Container Network In (B/s)', value: 'net_rx_bps', unit: 'B/s' },
    { id: 'c-net-out', category: 'Container', label: 'Container Network Out (B/s)', value: 'net_tx_bps', unit: 'B/s' },
    { id: 'h-cpu', category: 'Host', label: 'Host CPU Usage (%)', value: 'cpu_pct', unit: '%' },
    { id: 'h-mem', category: 'Host', label: 'Host Memory Usage (%)', value: 'mem_used_pct', unit: '%' },
    { id: 'h-disk', category: 'Host', label: 'Host Disk Usage (%)', value: 'disk_used_pct', unit: '%' },
    { id: 'h-net-in', category: 'Host', label: 'Host Network In (B/s)', value: 'net_rx_bps', unit: 'B/s' },
    { id: 'h-net-out', category: 'Host', label: 'Host Network Out (B/s)', value: 'net_tx_bps', unit: 'B/s' },
    { id: 'h-load1', category: 'Host', label: 'Host Load Average (1m)', value: 'load_1m', unit: 'Load' },
    { id: 'h-load5', category: 'Host', label: 'Host Load Average (5m)', value: 'load_5m', unit: 'Load' }
  ];

  const PRIMARY_METRIC_CARDS = [
    { key: 'cpu', tag: 'CPU', title: 'CPU Usage', subtitle: 'Processor utilization (%)', metric: 'cpu_pct', unit: '%' },
    { key: 'mem', tag: 'RAM', title: 'Memory Usage', subtitle: 'RAM utilization (%)', metric: 'mem_used_pct', unit: '%' },
    { key: 'disk', tag: 'DISK', title: 'Disk Usage', subtitle: 'Filesystem capacity (%)', metric: 'disk_used_pct', unit: '%' },
    { key: 'net', tag: 'NET', title: 'Network I/O', subtitle: 'Inbound throughput (B/s)', metric: 'net_rx_bps', unit: 'B/s' },
    { key: 'load', tag: 'LOAD', title: 'System Load', subtitle: '1-minute load average', metric: 'load_1m', unit: 'Load' }
  ];

  const DEFAULT_THRESHOLDS = {
    cpu_pct: 85,
    mem_used_pct: 90,
    mem_pct: 90,
    disk_used_pct: 85,
    net_rx_bps: 1000000,
    net_tx_bps: 1000000,
    mem_used: 1073741824,
    load_1m: 4.0,
    load_5m: 4.0
  };

  function normalizeMetricAlias(m) {
    if (m === 'container_cpu_pct' || m === 'host_cpu_pct') return 'cpu_pct';
    if (m === 'container_mem_pct') return 'mem_pct';
    if (m === 'host_mem_used_pct') return 'mem_used_pct';
    if (m === 'host_disk_used_pct') return 'disk_used_pct';
    return m || 'cpu_pct';
  }

  function defaultThresholdFor(metricName) {
    const norm = normalizeMetricAlias(metricName);
    return DEFAULT_THRESHOLDS[norm] !== undefined ? DEFAULT_THRESHOLDS[norm] : 85;
  }

  function isPrimaryMetricName(metricName) {
    return ['cpu_pct', 'mem_used_pct', 'mem_pct', 'disk_used_pct', 'net_rx_bps', 'load_1m'].includes(metricName);
  }

  function defaultForm() {
    return {
      id: null,
      name: '',
      kind: 'metric',
      target: 'host',
      severity: 'critical',
      for_sec: 30,
      cooldown_sec: 300,
      enabled: true,
      channels: {
        email: true,
        discord: false
      },
      metricName: 'cpu_pct',
      metricOp: '>',
      metricThreshold: 85,
      logPattern: '',
      logIsRegex: false,
      logThreshold: 5,
      logWindowSec: 60,
      logMinLevel: 5,
      containerEvents: {
        oom: true,
        die: false,
        restart: false,
        stop: false,
        down: false
      },
      containerThreshold: 1,
      containerWindowSec: 0
    };
  }

  function AlertsController(store) {
    let timer = null;
    let previewAbortController = null;
    let channelAutoPrefilled = false;

    function abortInflightPreview() {
      if (previewAbortController) {
        try {
          previewAbortController.abort();
        } catch (_) {}
        previewAbortController = null;
      }
    }

    return {
      activeAlerts: [],
      rules: [],
      templates: [],
      historyEvents: [],
      availableContainers: [],
      channelStatus: { email: false, discord: false },
      ruleSearch: '',
      activeTab: 'rules',
      historyLimit: 25,
      historySeverity: '',
      historyState: '',
      historyHasMore: false,
      historyLoading: false,

      modalOpen: false,
      isEditing: false,
      modalLoading: false,
      wizardStep: 1,
      maxStepReached: 1,
      targetMode: 'host',
      customMetricOpen: false,
      customNameEntered: false,
      stepError: '',

      metricDropdownOpen: false,
      metricSearchQuery: '',
      targetDropdownOpen: false,
      targetSearchQuery: '',
      ruleForm: defaultForm(),

      previewLoading: false,
      previewResult: null,
      previewError: '',
      previewTimedOut: false,

      primaryMetricCards: PRIMARY_METRIC_CARDS,

      mounted() {
        this.refresh();
        this.fetchTemplates();
        this.fetchContainers();
        this.fetchChannelStatus();
        this.startTimer();
        window.addEventListener('click', (e) => {
          if (!e.target.closest('.searchable-metric-wrap')) {
            this.metricDropdownOpen = false;
          }
          if (!e.target.closest('.searchable-target-wrap')) {
            this.targetDropdownOpen = false;
          }
        });
      },

      unmounted() {
        if (timer) {
          clearInterval(timer);
          timer = null;
        }
        abortInflightPreview();
      },

      startTimer() {
        if (timer) clearInterval(timer);
        const ms = { '5s': 5000, '10s': 10000, '30s': 30000 }[store.refreshInterval] || 10000;
        timer = setInterval(() => {
          if (store.route === 'alerts' || store.route === 'overview') {
            this.fetchActive();
            if (store.route === 'alerts' && this.activeTab === 'rules') this.fetchRules();
          }
        }, ms);
      },

      async refresh() {
        await Promise.all([this.fetchActive(), this.fetchRules(), this.fetchHistory(false)]);
      },

      async fetchActive() {
        try {
          const d = await store.apiFetch('/api/v1/alerts/active');
          this.activeAlerts = Array.isArray(d) ? d : [];
        } catch (_) {}
      },

      async fetchRules() {
        try {
          const d = await store.apiFetch('/api/v1/alerts/rules');
          this.rules = Array.isArray(d) ? d : [];
        } catch (_) {}
      },

      async fetchTemplates() {
        try {
          const d = await store.apiFetch('/api/v1/alerts/templates');
          this.templates = Array.isArray(d) ? d : [];
        } catch (_) {}
      },

      async fetchHistory(append = false) {
        if (this.historyLoading) return;
        this.historyLoading = true;
        try {
          let u = `/api/v1/alerts/history?limit=${this.historyLimit}`;
          if (this.historySeverity) u += `&severity=${encodeURIComponent(this.historySeverity)}`;
          if (this.historyState) u += `&state=${encodeURIComponent(this.historyState)}`;
          if (append && this.historyEvents.length) {
            const last = this.historyEvents[this.historyEvents.length - 1];
            if (last && last.started) u += `&cursor=${last.started}`;
          }
          const d = await store.apiFetch(u);
          const list = Array.isArray(d) ? d : [];
          this.historyEvents = append ? this.historyEvents.concat(list) : list;
          this.historyHasMore = list.length >= this.historyLimit;
        } catch (_) {
        } finally {
          this.historyLoading = false;
        }
      },

      filteredRules() {
        const q = (this.ruleSearch || '').trim().toLowerCase();
        if (!q) return this.rules;
        return this.rules.filter(r =>
          (r.name && r.name.toLowerCase().includes(q)) ||
          (r.target && r.target.toLowerCase().includes(q)) ||
          (r.kind && r.kind.toLowerCase().includes(q)) ||
          (r.severity && r.severity.toLowerCase().includes(q))
        );
      },

      async fetchContainers() {
        try {
          const d = await store.apiFetch('/api/v1/containers');
          if (d && Array.isArray(d.containers)) {
            this.availableContainers = d.containers.map(c => c.name).filter(Boolean);
          }
        } catch (_) {}
      },

      async fetchChannelStatus() {
        try {
          const cfg = await store.apiFetch('/api/v1/system/config');
          if (cfg && cfg.notifiers) {
            this.channelStatus.email = !!(cfg.notifiers.email && cfg.notifiers.email.enabled);
            this.channelStatus.discord = !!(cfg.notifiers.discord && cfg.notifiers.discord.enabled);
            if (!this.isEditing && this.modalOpen && !channelAutoPrefilled) {
              if (!this.channelStatus.email && this.channelStatus.discord) {
                this.ruleForm.channels.email = false;
                this.ruleForm.channels.discord = true;
              }
              channelAutoPrefilled = true;
            }
          }
        } catch (_) {}
      },

      isContainerTarget(t) {
        if (!t) return false;
        if (t === 'container:*') return true;
        if (t.startsWith('container:')) return true;
        return this.availableContainers.includes(t);
      },

      inferTargetMode(target) {
        const t = (target || '').trim();
        if (t === 'host') return 'host';
        if (t === 'container:*') return 'all_containers';
        if (t && (this.availableContainers.includes(t) || (t.startsWith('container:') && t !== 'container:*'))) {
          return 'container';
        }
        return 'custom';
      },

      setKind(kind) {
        this.stepError = '';
        this.ruleForm.kind = kind;
        if (kind === 'container' && this.ruleForm.target === 'host') {
          this.selectTargetMode('all_containers');
        }
        this.syncDefaultNameIfAuto();
      },

      isMetricCardActive(cardKey) {
        if (this.customMetricOpen) return false;
        const m = this.ruleForm.metricName;
        if (cardKey === 'cpu') return m === 'cpu_pct';
        if (cardKey === 'mem') return m === 'mem_used_pct' || m === 'mem_pct';
        if (cardKey === 'disk') return m === 'disk_used_pct';
        if (cardKey === 'net') return m === 'net_rx_bps';
        if (cardKey === 'load') return m === 'load_1m';
        return false;
      },

      selectMetricCard(cardKey) {
        this.stepError = '';
        this.ruleForm.kind = 'metric';
        this.customMetricOpen = false;
        this.metricDropdownOpen = false;
        const prevMetric = this.ruleForm.metricName;
        let nextMetric = prevMetric;

        if (cardKey === 'cpu') {
          nextMetric = 'cpu_pct';
          if ((this.ruleForm.target || '').startsWith('disk:')) {
            this.selectTargetMode('host');
          }
        } else if (cardKey === 'mem') {
          nextMetric = this.isContainerTarget(this.ruleForm.target) || this.targetMode === 'container' || this.targetMode === 'all_containers'
            ? 'mem_pct'
            : 'mem_used_pct';
          if ((this.ruleForm.target || '').startsWith('disk:')) {
            this.selectTargetMode('host');
          }
        } else if (cardKey === 'disk') {
          nextMetric = 'disk_used_pct';
          if (!(this.ruleForm.target || '').startsWith('disk:') && this.ruleForm.target !== '*') {
            this.targetMode = 'custom';
            this.ruleForm.target = 'disk:*';
          }
        } else if (cardKey === 'net') {
          nextMetric = 'net_rx_bps';
          if ((this.ruleForm.target || '').startsWith('disk:')) {
            this.selectTargetMode('host');
          }
        } else if (cardKey === 'load') {
          nextMetric = 'load_1m';
          this.targetMode = 'host';
          this.ruleForm.target = 'host';
        }

        this.ruleForm.metricName = nextMetric;
        if (!this.isEditing && prevMetric !== nextMetric) {
          this.ruleForm.metricThreshold = defaultThresholdFor(nextMetric);
        }
        this.syncDefaultNameIfAuto();
      },

      openCustomMetricPicker() {
        this.stepError = '';
        this.ruleForm.kind = 'metric';
        this.customMetricOpen = true;
      },

      selectTargetMode(mode) {
        this.stepError = '';
        this.targetDropdownOpen = false;
        this.targetMode = mode;

        if (mode === 'host') {
          this.setTarget('host');
        } else if (mode === 'all_containers') {
          this.setTarget('container:*');
        } else if (mode === 'container') {
          const cur = (this.ruleForm.target || '').trim();
          const curStripped = cur.startsWith('container:') ? cur.slice('container:'.length) : cur;
          if (this.availableContainers.includes(curStripped)) {
            this.setTarget(curStripped);
          } else if (this.availableContainers.length > 0) {
            this.setTarget(this.availableContainers[0]);
          } else {
            this.ruleForm.target = '';
          }
        } else if (mode === 'custom') {
          if (!this.ruleForm.target || this.ruleForm.target === 'host' || this.ruleForm.target === 'container:*') {
            this.ruleForm.target = this.ruleForm.metricName === 'disk_used_pct' ? 'disk:*' : '*';
          }
        }
        this.syncDefaultNameIfAuto();
      },

      setTarget(t) {
        this.stepError = '';
        this.ruleForm.target = t;
        if (t === 'host') {
          if (this.ruleForm.metricName === 'mem_pct') {
            this.ruleForm.metricName = 'mem_used_pct';
          }
        } else if (t && (t.startsWith('container:') || this.availableContainers.includes(t))) {
          if (this.ruleForm.metricName === 'mem_used_pct') {
            this.ruleForm.metricName = 'mem_pct';
          } else if (this.ruleForm.metricName === 'disk_used_pct' || this.ruleForm.metricName === 'load_1m' || this.ruleForm.metricName === 'load_5m') {
            this.ruleForm.metricName = 'cpu_pct';
            if (!this.isEditing) {
              this.ruleForm.metricThreshold = defaultThresholdFor('cpu_pct');
            }
          }
        }
        this.syncDefaultNameIfAuto();
      },

      getMetricUnit() {
        const m = this.ruleForm.metricName || '';
        if (m.includes('pct')) return '%';
        if (m.includes('bps')) return 'B/s';
        if (m.includes('bytes') || m === 'mem_used') return 'Bytes';
        if (m.startsWith('load')) return 'Load';
        return '';
      },

      currentMetric() {
        const val = this.ruleForm.metricName;
        const isHost = this.ruleForm.target === 'host';
        let found = METRIC_OPTIONS.find(m => m.value === val && (isHost ? m.category === 'Host' : m.category === 'Container'));
        if (!found) found = METRIC_OPTIONS.find(m => m.value === val);
        return found || { category: 'Custom', label: val || 'Select Metric', value: val, unit: this.getMetricUnit() };
      },

      toggleMetricDropdown() {
        this.metricDropdownOpen = !this.metricDropdownOpen;
        if (this.metricDropdownOpen) {
          this.metricSearchQuery = '';
          this.targetDropdownOpen = false;
          setTimeout(() => {
            const el = document.getElementById('alert-metric-search-input');
            if (el) el.focus();
          }, 40);
        }
      },

      filteredMetrics() {
        const q = (this.metricSearchQuery || '').trim().toLowerCase();
        if (!q) return METRIC_OPTIONS;
        return METRIC_OPTIONS.filter(m =>
          m.label.toLowerCase().includes(q) ||
          m.value.toLowerCase().includes(q) ||
          m.category.toLowerCase().includes(q)
        );
      },

      selectMetric(m) {
        if (!m) return;
        const prevMetric = this.ruleForm.metricName;
        this.ruleForm.metricName = m.value;
        this.metricDropdownOpen = false;
        this.metricSearchQuery = '';
        this.customMetricOpen = !isPrimaryMetricName(m.value);
        if (m.value === 'load_1m' || m.value === 'load_5m') {
          this.targetMode = 'host';
          this.ruleForm.target = 'host';
        } else if (m.value === 'disk_used_pct' && !(this.ruleForm.target || '').startsWith('disk:') && this.ruleForm.target !== '*') {
          this.targetMode = 'custom';
          this.ruleForm.target = 'disk:*';
        }
        if (!this.isEditing && prevMetric !== m.value) {
          this.ruleForm.metricThreshold = defaultThresholdFor(m.value);
        }
        this.syncDefaultNameIfAuto();
      },

      allTargetOptions() {
        const list = [
          { category: 'Host', label: 'Host System (host)', value: 'host', badgeClass: 'badge-secondary' },
          { category: 'Wildcard', label: 'All Monitored Targets (*)', value: '*', badgeClass: 'badge-secondary' },
          { category: 'Wildcard', label: 'All Containers (container:*)', value: 'container:*', badgeClass: 'badge-primary' },
          { category: 'Disk', label: 'All Mounted Disks (disk:*)', value: 'disk:*', badgeClass: 'badge-secondary' },
          { category: 'Disk', label: 'Root Filesystem (disk:/)', value: 'disk:/', badgeClass: 'badge-secondary' }
        ];
        (this.availableContainers || []).forEach(c => {
          list.push({ category: 'Container', label: `Container: ${c}`, value: c, badgeClass: 'badge-primary' });
        });
        return list;
      },

      filteredTargets() {
        const q = (this.targetSearchQuery || '').trim().toLowerCase();
        const all = this.allTargetOptions();
        if (!q) return all;
        return all.filter(t =>
          t.label.toLowerCase().includes(q) ||
          t.value.toLowerCase().includes(q) ||
          t.category.toLowerCase().includes(q)
        );
      },

      hasExactTargetMatch() {
        const q = (this.targetSearchQuery || '').trim().toLowerCase();
        if (!q) return true;
        return this.allTargetOptions().some(t => t.value.toLowerCase() === q);
      },

      toggleTargetDropdown() {
        this.targetDropdownOpen = !this.targetDropdownOpen;
        if (this.targetDropdownOpen) {
          this.targetSearchQuery = '';
          this.metricDropdownOpen = false;
          setTimeout(() => {
            const el = document.getElementById('alert-target-search-input');
            if (el) el.focus();
          }, 40);
        }
      },

      selectTarget(t) {
        if (!t) return;
        this.targetMode = this.inferTargetMode(t);
        this.setTarget(t);
        this.targetDropdownOpen = false;
        this.targetSearchQuery = '';
      },

      validateStep1() {
        const f = this.ruleForm;
        const target = (f.target || '').trim();
        if (!target) {
          return 'Please select or enter a target before continuing.';
        }
        if (f.kind === 'metric' && !(f.metricName || '').trim()) {
          return 'Please choose a metric to monitor.';
        }
        return '';
      },

      validateStep2() {
        const f = this.ruleForm;
        if (f.kind === 'metric') {
          const val = parseFloat(f.metricThreshold);
          if (f.metricThreshold === '' || f.metricThreshold === null || isNaN(val)) {
            return 'Please enter a valid numeric threshold.';
          }
        } else if (f.kind === 'log') {
          if (!(f.logPattern || '').trim()) {
            return 'Please enter a log pattern to match.';
          }
          const win = parseInt(f.logWindowSec, 10);
          if (isNaN(win) || win < 10) {
            return 'Log window must be at least 10 seconds.';
          }
          const hits = parseInt(f.logThreshold, 10);
          if (isNaN(hits) || hits < 1) {
            return 'Log hit threshold must be at least 1.';
          }
        } else if (f.kind === 'container') {
          const evs = Object.keys(f.containerEvents).filter(k => f.containerEvents[k]);
          if (!evs.length) {
            return 'Select at least one container lifecycle event.';
          }
        }
        const forSec = parseInt(f.for_sec, 10);
        if (isNaN(forSec) || forSec < 0 || forSec > 86400) {
          return 'Sustained duration must be between 0 and 86400 seconds.';
        }
        if (!this.selectedChannels().length) {
          return 'Select at least one notification channel (Email or Discord).';
        }
        return '';
      },

      canProceedToStep2() {
        return !this.validateStep1();
      },

      canProceedToStep3() {
        return !this.validateStep1() && !this.validateStep2();
      },

      canJumpToStep(step) {
        if (step === this.wizardStep) return true;
        if (step < this.wizardStep) return true;
        if (this.isEditing || step <= this.maxStepReached) {
          if (step === 2) return this.canProceedToStep2();
          if (step === 3) return this.canProceedToStep3();
        }
        return false;
      },

      goToStep(step) {
        this.stepError = '';
        this.metricDropdownOpen = false;
        this.targetDropdownOpen = false;

        if (step === this.wizardStep) return;
        if (step >= 2) {
          const err1 = this.validateStep1();
          if (err1) {
            this.stepError = err1;
            store.toast.show(err1, 'warning');
            return;
          }
        }
        if (step >= 3) {
          const err2 = this.validateStep2();
          if (err2) {
            this.stepError = err2;
            store.toast.show(err2, 'warning');
            return;
          }
        }

        if (this.wizardStep === 3 && step < 3) {
          abortInflightPreview();
          this.previewLoading = false;
        }

        this.wizardStep = step;
        if (step > this.maxStepReached) {
          this.maxStepReached = step;
        }

        if (step === 3) {
          this.syncDefaultNameIfAuto();
          this.runPreview();
        }
      },

      nextStep() {
        if (this.wizardStep === 1) {
          this.goToStep(2);
        } else if (this.wizardStep === 2) {
          this.goToStep(3);
        }
      },

      prevStep() {
        if (this.wizardStep > 1) {
          this.goToStep(this.wizardStep - 1);
        }
      },

      onNameInput() {
        this.customNameEntered = !!(this.ruleForm.name && this.ruleForm.name.trim());
      },

      resetDefaultName() {
        this.customNameEntered = false;
        this.ruleForm.name = this.generateDefaultName();
      },

      syncDefaultNameIfAuto() {
        if (!this.customNameEntered) {
          this.ruleForm.name = this.generateDefaultName();
        }
      },

      formatTargetLabel(target) {
        const t = (target || '').trim();
        if (!t) return 'Target';
        if (t === 'host') return 'Host';
        if (t === 'container:*') return 'All Containers';
        if (t === '*') return 'All Targets';
        if (t === 'disk:*') return 'All Disks';
        if (t.startsWith('disk:')) return `Disk ${t.slice(5)}`;
        if (t.startsWith('container:')) return t.slice('container:'.length);
        return t;
      },

      shortMetricLabel(metricName) {
        const m = normalizeMetricAlias(metricName);
        const map = {
          cpu_pct: 'CPU',
          mem_used_pct: 'Memory',
          mem_pct: 'Memory',
          disk_used_pct: 'Disk',
          net_rx_bps: 'Net In',
          net_tx_bps: 'Net Out',
          mem_used: 'Memory Used',
          load_1m: 'Load (1m)',
          load_5m: 'Load (5m)'
        };
        return map[m] || m;
      },

      generateDefaultName() {
        const f = this.ruleForm;
        const targetLabel = this.formatTargetLabel(f.target);
        const forSec = parseInt(f.for_sec, 10) || 0;
        const durSuffix = forSec > 0 ? ` for ${fmtDur(forSec)}` : '';
        let name = '';

        if (f.kind === 'metric') {
          const metricLabel = this.shortMetricLabel(f.metricName);
          const unit = this.getMetricUnit();
          const unitStr = unit === '%' ? '%' : (unit && unit !== 'Load' ? ` ${unit}` : '');
          const thr = f.metricThreshold !== '' && f.metricThreshold != null ? f.metricThreshold : defaultThresholdFor(f.metricName);
          name = `${targetLabel} ${metricLabel} ${f.metricOp || '>'} ${thr}${unitStr}${durSuffix}`;
        } else if (f.kind === 'log') {
          const pat = (f.logPattern || '').trim() || 'pattern';
          const shortPat = pat.length > 24 ? pat.slice(0, 21) + '...' : pat;
          name = `${targetLabel} Log "${shortPat}" >= ${f.logThreshold || 1} in ${f.logWindowSec || 60}s`;
        } else if (f.kind === 'container') {
          const evs = Object.keys(f.containerEvents).filter(k => f.containerEvents[k]).map(e => e.toUpperCase());
          const evStr = evs.length ? evs.join(', ') : 'Event';
          name = `${targetLabel} ${evStr}${durSuffix}`;
        } else {
          name = `${targetLabel} Alert`;
        }

        return name.slice(0, 120);
      },

      conditionSummaryText() {
        const f = this.ruleForm;
        if (f.kind === 'metric') {
          const unit = this.getMetricUnit();
          const unitStr = unit ? ` ${unit}` : '';
          return `${this.currentMetric().label} (${f.metricName}) ${f.metricOp} ${f.metricThreshold}${unitStr}`;
        }
        if (f.kind === 'log') {
          return `Pattern "${f.logPattern}"${f.logIsRegex ? ' (regex)' : ''} ≥ ${f.logThreshold} hits in ${f.logWindowSec}s`;
        }
        if (f.kind === 'container') {
          const evs = Object.keys(f.containerEvents).filter(k => f.containerEvents[k]).map(e => e.toUpperCase());
          return `Events [${evs.join(', ')}] ≥ ${f.containerThreshold} in ${fmtDur(f.containerWindowSec)}`;
        }
        return '--';
      },

      isNoTelemetryPreview() {
        if (!this.previewResult || this.previewResult.would_fire_now) return false;
        if (this.ruleForm.kind !== 'metric') return false;
        const lines = this.previewResult.sample_lines || [];
        return lines.some(l =>
          l.includes('No active telemetry') ||
          l.includes('no sample received within last 120s')
        );
      },

      openCreateModal() {
        abortInflightPreview();
        this.isEditing = false;
        this.wizardStep = 1;
        this.maxStepReached = 1;
        this.targetMode = 'host';
        this.customMetricOpen = false;
        this.customNameEntered = false;
        this.stepError = '';
        this.previewLoading = false;
        this.previewResult = null;
        this.previewError = '';
        this.previewTimedOut = false;
        this.metricDropdownOpen = false;
        this.targetDropdownOpen = false;
        channelAutoPrefilled = false;
        this.ruleForm = defaultForm();
        this.fetchContainers();
        this.fetchChannelStatus();
        this.modalOpen = true;
      },

      openEditModal(rule) {
        abortInflightPreview();
        this.isEditing = true;
        this.wizardStep = 1;
        this.maxStepReached = 3;
        this.customNameEntered = true;
        this.stepError = '';
        this.previewLoading = false;
        this.previewResult = null;
        this.previewError = '';
        this.previewTimedOut = false;
        this.metricDropdownOpen = false;
        this.targetDropdownOpen = false;
        channelAutoPrefilled = true;
        this.fetchContainers();
        this.fetchChannelStatus();

        const form = defaultForm();
        form.id = rule.id;
        form.name = rule.name || '';
        form.kind = rule.kind || 'metric';
        form.target = rule.target || 'host';
        form.severity = rule.severity || 'critical';
        form.for_sec = rule.for_sec != null ? rule.for_sec : 30;
        form.cooldown_sec = rule.cooldown_sec != null ? rule.cooldown_sec : 300;
        form.enabled = rule.enabled !== false;
        form.channels = { email: false, discord: false };
        (rule.channels || []).forEach(ch => {
          const lc = ch.toLowerCase();
          if (form.channels[lc] !== undefined) form.channels[lc] = true;
        });

        try {
          const c = JSON.parse(rule.condition_json || '{}');
          if (rule.kind === 'metric') {
            const m = normalizeMetricAlias(c.metric);
            form.metricName = m;
            form.metricOp = c.operator || '>';
            form.metricThreshold = c.threshold != null ? c.threshold : defaultThresholdFor(m);
          } else if (rule.kind === 'log') {
            form.logPattern = c.pattern || '';
            form.logIsRegex = !!c.is_regex;
            form.logThreshold = c.threshold || 5;
            form.logWindowSec = c.window_sec || 60;
            form.logMinLevel = c.min_level != null ? c.min_level : 5;
          } else if (rule.kind === 'container') {
            form.containerEvents = { oom: false, die: false, restart: false, stop: false, down: false };
            (c.events || []).forEach(e => {
              if (form.containerEvents[e] !== undefined) form.containerEvents[e] = true;
            });
            form.containerThreshold = c.threshold || 1;
            form.containerWindowSec = c.window_sec || 0;
          }
        } catch (_) {}

        this.ruleForm = form;
        this.targetMode = this.inferTargetMode(form.target);
        this.customMetricOpen = form.kind === 'metric' && !isPrimaryMetricName(form.metricName);
        this.modalOpen = true;
      },

      closeModal() {
        abortInflightPreview();
        this.modalOpen = false;
        this.previewLoading = false;
        this.previewResult = null;
        this.previewError = '';
        this.previewTimedOut = false;
        this.stepError = '';
        this.metricDropdownOpen = false;
        this.targetDropdownOpen = false;
      },

      buildConditionJSON() {
        const f = this.ruleForm;
        if (f.kind === 'metric') {
          const m = normalizeMetricAlias(f.metricName);
          return JSON.stringify({
            metric: m,
            operator: f.metricOp || '>',
            threshold: parseFloat(f.metricThreshold)
          });
        }
        if (f.kind === 'log') {
          return JSON.stringify({
            pattern: f.logPattern,
            is_regex: !!f.logIsRegex,
            threshold: parseInt(f.logThreshold, 10) || 1,
            window_sec: parseInt(f.logWindowSec, 10) || 60,
            min_level: parseInt(f.logMinLevel, 10) || 0
          });
        }
        if (f.kind === 'container') {
          const evs = Object.keys(f.containerEvents).filter(k => f.containerEvents[k]);
          return JSON.stringify({
            events: evs,
            threshold: parseInt(f.containerThreshold, 10) || 1,
            window_sec: parseInt(f.containerWindowSec, 10) || 0
          });
        }
        return '{}';
      },

      selectedChannels() {
        return Object.keys(this.ruleForm.channels).filter(k => this.ruleForm.channels[k]);
      },

      async runPreview() {
        if (!this.ruleForm.target || !this.ruleForm.target.trim()) {
          this.previewError = 'Target is required for dry-run evaluation.';
          return;
        }
        abortInflightPreview();
        const controller = new AbortController();
        previewAbortController = controller;
        let timedOut = false;
        const timeoutId = setTimeout(() => {
          timedOut = true;
          controller.abort();
        }, DRY_RUN_TIMEOUT_MS);

        this.previewLoading = true;
        this.previewResult = null;
        this.previewError = '';
        this.previewTimedOut = false;

        try {
          const res = await fetch('/api/v1/alerts/rules/preview', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            signal: controller.signal,
            body: JSON.stringify({
              kind: this.ruleForm.kind,
              target: this.ruleForm.target.trim(),
              severity: this.ruleForm.severity,
              condition_json: this.buildConditionJSON(),
              for_sec: parseInt(this.ruleForm.for_sec, 10) || 0,
              channels: this.selectedChannels()
            })
          });

          if (res.status === 401) {
            store.authenticated = false;
            store.user = '';
            store.toast.show('Session expired. Please log in again.', 'warning');
            this.previewError = 'Session expired. Please sign in again.';
            return;
          }

          const data = await res.json().catch(() => ({}));
          if (!res.ok) {
            const errMsg = (data && data.error && data.error.message) || ('Preview failed (HTTP ' + res.status + ')');
            this.previewError = errMsg;
            return;
          }

          if (previewAbortController === controller) {
            this.previewResult = data;
          }
        } catch (err) {
          if (timedOut) {
            this.previewTimedOut = true;
            this.previewError = 'Dry-run evaluation timed out after 60s. Live evaluation could not complete in time.';
          } else if (err && err.name === 'AbortError') {
            return;
          } else {
            this.previewError = (err && err.message) ? err.message : 'Dry-run evaluation failed.';
          }
        } finally {
          clearTimeout(timeoutId);
          if (previewAbortController === controller) {
            previewAbortController = null;
            this.previewLoading = false;
          }
        }
      },

      handleFormSubmit() {
        if (this.wizardStep < 3) {
          this.nextStep();
          return;
        }
        this.saveRule();
      },

      async saveRule() {
        const f = this.ruleForm;
        if (!f.name || !f.name.trim()) {
          this.stepError = 'Rule name is required.';
          return store.toast.show('Rule name required', 'warning');
        }
        const err1 = this.validateStep1();
        if (err1) {
          this.stepError = err1;
          return store.toast.show(err1, 'warning');
        }
        const err2 = this.validateStep2();
        if (err2) {
          this.stepError = err2;
          return store.toast.show(err2, 'warning');
        }

        const chs = this.selectedChannels();
        this.stepError = '';
        this.modalLoading = true;
        try {
          const body = {
            name: f.name.trim(),
            kind: f.kind,
            target: f.target.trim(),
            severity: f.severity,
            condition_json: this.buildConditionJSON(),
            for_sec: parseInt(f.for_sec, 10) || 0,
            cooldown_sec: parseInt(f.cooldown_sec, 10) || 300,
            channels: chs,
            enabled: !!f.enabled
          };
          if (this.isEditing) {
            body.id = f.id;
            await store.apiFetch(`/api/v1/alerts/rules/${f.id}`, {
              method: 'PUT',
              body
            });
            store.toast.show(`Rule "${body.name}" updated`, 'success');
          } else {
            await store.apiFetch('/api/v1/alerts/rules', {
              method: 'POST',
              body
            });
            store.toast.show(`Rule "${body.name}" created`, 'success');
          }
          this.closeModal();
          await this.fetchRules();
        } catch (_) {
        } finally {
          this.modalLoading = false;
        }
      },

      async toggleRule(r) {
        const action = r.enabled ? 'disable' : 'enable';
        try {
          await store.apiFetch(`/api/v1/alerts/rules/${r.id}/${action}`, { method: 'POST' });
          r.enabled = !r.enabled;
          store.toast.show(`Rule "${r.name}" ${action}d`, 'info');
        } catch (_) {}
      },

      async deleteRule(r) {
        if (!confirm(`Delete rule "${r.name}"?`)) return;
        try {
          await store.apiFetch(`/api/v1/alerts/rules/${r.id}`, { method: 'DELETE' });
          store.toast.show(`Rule "${r.name}" deleted`, 'info');
          await this.fetchRules();
        } catch (_) {}
      },

      applyTemplate(tmpl) {
        if (!tmpl) return;
        this.openCreateModal();
        const r = tmpl.rule;
        this.ruleForm.name = r.name || '';
        this.ruleForm.kind = r.kind || 'metric';
        this.ruleForm.target = r.target || 'host';
        this.ruleForm.severity = r.severity || 'warning';
        this.ruleForm.for_sec = r.for_sec != null ? r.for_sec : 30;
        this.ruleForm.cooldown_sec = r.cooldown_sec != null ? r.cooldown_sec : 300;
        if (Array.isArray(r.channels) && r.channels.length) {
          this.ruleForm.channels = { email: false, discord: false };
          r.channels.forEach(ch => {
            const lc = ch.toLowerCase();
            if (this.ruleForm.channels[lc] !== undefined) this.ruleForm.channels[lc] = true;
          });
          channelAutoPrefilled = true;
        }
        try {
          const c = JSON.parse(r.condition_json || '{}');
          if (r.kind === 'metric') {
            this.ruleForm.metricName = normalizeMetricAlias(c.metric);
            this.ruleForm.metricOp = c.operator || '>';
            this.ruleForm.metricThreshold = c.threshold != null ? c.threshold : 85;
          } else if (r.kind === 'log') {
            this.ruleForm.logPattern = c.pattern || '';
            this.ruleForm.logIsRegex = !!c.is_regex;
            this.ruleForm.logThreshold = c.threshold || 5;
            this.ruleForm.logWindowSec = c.window_sec || 60;
            this.ruleForm.logMinLevel = c.min_level != null ? c.min_level : 5;
          } else if (r.kind === 'container') {
            this.ruleForm.containerEvents = { oom: false, die: false, restart: false, stop: false, down: false };
            (c.events || []).forEach(e => {
              if (this.ruleForm.containerEvents[e] !== undefined) this.ruleForm.containerEvents[e] = true;
            });
            this.ruleForm.containerThreshold = c.threshold || 1;
            this.ruleForm.containerWindowSec = c.window_sec || 0;
          }
        } catch (_) {}

        this.customNameEntered = true;
        this.targetMode = this.inferTargetMode(this.ruleForm.target);
        this.customMetricOpen = this.ruleForm.kind === 'metric' && !isPrimaryMetricName(this.ruleForm.metricName);
        this.maxStepReached = 3;
        this.wizardStep = 3;
        store.toast.show(`Loaded template "${tmpl.name}"`, 'info');
        this.runPreview();
      },

      async exportRules() {
        try {
          const b = await store.apiFetch('/api/v1/alerts/rules/export');
          const blob = new Blob([JSON.stringify(b, null, 2)], { type: 'application/json' });
          const url = URL.createObjectURL(blob);
          const a = document.createElement('a');
          a.href = url;
          a.download = `tarsier-alerts-${new Date().toISOString().slice(0, 10)}.json`;
          document.body.appendChild(a);
          a.click();
          document.body.removeChild(a);
          URL.revokeObjectURL(url);
          store.toast.show('Exported rules JSON', 'success');
        } catch (_) {}
      },

      async importFile(e) {
        const file = e.target.files && e.target.files[0];
        if (!file) return;
        try {
          const text = await file.text();
          const parsed = JSON.parse(text);
          const res = await store.apiFetch('/api/v1/alerts/rules/import', {
            method: 'POST',
            body: parsed
          });
          store.toast.show(`Imported ${res.imported} rules`, 'success');
          await this.fetchRules();
        } catch (err) {
          store.toast.show('Import failed: ' + err.message, 'error');
        } finally {
          e.target.value = '';
        }
      },

      async sendTest(channel) {
        try {
          await store.apiFetch('/api/v1/alerts/test', {
            method: 'POST',
            body: { channel }
          });
          store.toast.show(`Test alert dispatched over ${channel}`, 'success');
        } catch (_) {}
      },

      formatCondition(r) {
        if (!r.condition_json) return '--';
        try {
          const c = JSON.parse(r.condition_json);
          if (r.kind === 'metric') return `${c.metric} ${c.operator} ${c.threshold}`;
          if (r.kind === 'log') return `"${c.pattern}" ${c.is_regex ? '(regex) ' : ''}≥ ${c.threshold} in ${c.window_sec}s`;
          if (r.kind === 'container') return `events:[${(c.events || []).join(',')}] (≥${c.threshold})`;
        } catch (_) {}
        return r.condition_json;
      },

      formatTime: fmtTime,
      formatDuration: fmtDur
    };
  }

  window.AlertsController = AlertsController;
})();
