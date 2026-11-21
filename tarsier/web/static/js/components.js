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
function StatCard(title, value, unit, change) {
return {
title: title || '',
value: value != null ? value : '--',
unit: unit || '',
change: change || ''
};
}
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
function ChartPanel(containerId) {
return {
mounted(el) {
const C = window.TarsierCharts;
if (!C || !el) return;
C.mountChart(el, containerId, {
title: '',
range: [0, 100],
valFmt: v => (v != null ? v.toFixed(1) + '%' : '--'),
series: [
{ label: 'CPU %', stroke: '#2563eb', fill: 'rgba(37, 99, 235, 0.1)' },
{ label: 'RAM %', stroke: '#16a34a', fill: 'rgba(22, 163, 74, 0.1)' }
]
});
if (window.hostView && window.hostView.updateOverviewChart) {
window.hostView.updateOverviewChart();
}
},
unmounted() {
if (window.TarsierCharts) {
window.TarsierCharts.unmountChart(containerId);
}
}
};
}
window.StatusBadge = StatusBadge;
window.StatCard = StatCard;
window.ToastState = ToastState;
window.ChartPanel = ChartPanel;