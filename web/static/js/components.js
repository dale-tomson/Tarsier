/**
 * Tarsier Reusable Petite-Vue Component State Factories & Lifecycle Hooks
 */

// StatusBadge computes semantic CSS classes for service/container statuses
function StatusBadge(status) {
  return {
    status: status || 'unknown',
    get label() {
      return (this.status || 'unknown').toUpperCase();
    },
    get badgeClass() {
      const s = (this.status || '').toLowerCase();
      if (s === 'running' || s === 'ok' || s === 'healthy' || s === 'up') return 'badge-success';
      if (s === 'warning' || s === 'degraded' || s === 'restarting') return 'badge-warning';
      if (s === 'critical' || s === 'error' || s === 'down' || s === 'failed') return 'badge-danger';
      return 'badge-muted';
    }
  };
}

// StatCard manages reactive state for metric cards
function StatCard(title, value, unit, change) {
  return {
    title: title || '',
    value: value != null ? value : '--',
    unit: unit || '',
    change: change || ''
  };
}

// Toast notification state factory
function ToastState() {
  return {
    message: '',
    type: 'info',
    visible: false,
    _timer: null,
    show(msg, type = 'info', timeoutMs = 4000) {
      this.message = msg;
      this.type = type;
      this.visible = true;
      if (this._timer) clearTimeout(this._timer);
      if (timeoutMs > 0) {
        this._timer = setTimeout(() => {
          this.visible = false;
        }, timeoutMs);
      }
    },
    dismiss() {
      this.visible = false;
      if (this._timer) clearTimeout(this._timer);
    }
  };
}

// ChartPanel wraps uPlot lifecycle for @vue:mounted and @vue:unmounted
function ChartPanel(containerId, options, initialData) {
  return {
    chart: null,
    mounted(el) {
      if (!window.uPlot) return;
      const target = el || document.getElementById(containerId);
      if (!target) return;

      const rect = target.getBoundingClientRect();
      const width = Math.floor(rect.width) || 600;
      const height = 220;

      const opts = options || {
        title: "System CPU & Memory (Sample)",
        width: width,
        height: height,
        series: [
          {},
          { label: "CPU %", stroke: "#2563eb", width: 2 },
          { label: "RAM %", stroke: "#16a34a", width: 2 }
        ],
        axes: [
          {},
          {
            label: "%",
            values: (u, vals) => vals.map(v => v != null ? v.toFixed(1) + "%" : "")
          }
        ]
      };

      const now = Math.floor(Date.now() / 1000);
      const data = initialData || [
        [now - 300, now - 240, now - 180, now - 120, now - 60, now],
        [12.5, 15.2, 11.0, 18.4, 13.8, 14.2],
        [42.1, 42.4, 42.8, 43.1, 43.0, 42.9]
      ];

      this.chart = new window.uPlot(opts, data, target);

      // Handle window resize cleanly
      this._onResize = () => {
        if (this.chart && target) {
          const w = Math.floor(target.getBoundingClientRect().width);
          if (w > 0) this.chart.setSize({ width: w, height: height });
        }
      };
      window.addEventListener('resize', this._onResize);
    },
    unmounted() {
      if (this._onResize) {
        window.removeEventListener('resize', this._onResize);
        this._onResize = null;
      }
      if (this.chart) {
        this.chart.destroy();
        this.chart = null;
      }
    }
  };
}

// Export to window
window.StatusBadge = StatusBadge;
window.StatCard = StatCard;
window.ToastState = ToastState;
window.ChartPanel = ChartPanel;
