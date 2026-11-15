(function() {
'use strict';
function formatBytes(bytes) {
if (bytes == null || isNaN(bytes) || bytes === 0) return '0 B';
const k = 1024;
const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
const i = Math.floor(Math.log(bytes) / Math.log(k));
return (bytes / Math.pow(k, i)).toFixed(i === 0 ? 0 : 1) + ' ' + (sizes[i] || 'TB');
}
function formatBps(bps) {
if (bps == null || isNaN(bps) || bps === 0) return '0 B/s';
const k = 1024;
const sizes = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
const i = Math.floor(Math.log(bps) / Math.log(k));
return (bps / Math.pow(k, i)).toFixed(1) + ' ' + (sizes[i] || 'GB/s');
}
function parseTimeRangeToSeconds(range) {
switch (range) {
case '6h': return 6 * 3600;
case '24h': return 24 * 3600;
case '7d': return 7 * 86400;
case '30d': return 30 * 86400;
case '1h':
default: return 3600;
}
}
function alignChartData(seriesLists) {
const timeSet = new Set();
seriesLists.forEach(list => {
(list || []).forEach(p => timeSet.add(p.ts));
});
const timestamps = Array.from(timeSet).sort((a, b) => a - b);
const timeIndexMap = new Map();
timestamps.forEach((ts, idx) => timeIndexMap.set(ts, idx));
const result = [timestamps];
seriesLists.forEach(list => {
const vals = new Array(timestamps.length).fill(null);
(list || []).forEach(p => {
const idx = timeIndexMap.get(p.ts);
if (idx !== undefined) {
vals[idx] = p.v;
}
});
result.push(vals);
});
return result;
}
function HostController(appStore) {
return {
store: appStore,
summary: {
cpu: { avg_pct: 0, peak_pct: 0, cores: 0 },
ram: { avg_bytes: 0, peak_bytes: 0, total_bytes: 0 }
},
current: {
cpuPct: '--',
memUsed: '--',
memAvail: '--',
load1: '--',
load5: '--',
load15: '--',
diskTotal: '--',
diskUsed: '--',
diskAvail: '--',
diskUsedPct: '--'
},
history: [],
mounts: [],
_timer: null,
formatBytes,
formatBps,
async mounted() {
this.initCharts();
await this.refresh();
this.startRefreshTimer();
},
unmounted() {
this.stopRefreshTimer();
const chartIds = ['chart-cpu', 'chart-load', 'chart-mem', 'chart-net', 'chart-disk-io', 'chart-disk-space'];
chartIds.forEach(id => window.TarsierCharts.unmountChart(id));
},
initCharts() {
const C = window.TarsierCharts;
const cpuEl = document.getElementById('chart-cpu');
if (cpuEl) {
C.mountChart(cpuEl, 'chart-cpu', {
title: 'CPU Utilization (%)',
range: [0, 100],
valFmt: v => v.toFixed(1) + '%',
series: [{ label: 'CPU %', stroke: '#2563eb', fill: 'rgba(37, 99, 235, 0.1)' }]
});
}
const loadEl = document.getElementById('chart-load');
if (loadEl) {
C.mountChart(loadEl, 'chart-load', {
title: 'System Load Average',
valFmt: v => v.toFixed(2),
series: [
{ label: '1m', stroke: '#2563eb' },
{ label: '5m', stroke: '#0891b2' },
{ label: '15m', stroke: '#64748b' }
]
});
}
const memEl = document.getElementById('chart-mem');
if (memEl) {
C.mountChart(memEl, 'chart-mem', {
title: 'Memory & Swap',
valFmt: v => formatBytes(v),
series: [
{ label: 'Used', stroke: '#ea580c', fill: 'rgba(234, 88, 12, 0.1)' },
{ label: 'Available', stroke: '#16a34a' },
{ label: 'Swap Used', stroke: '#9333ea' }
]
});
}
const netEl = document.getElementById('chart-net');
if (netEl) {
C.mountChart(netEl, 'chart-net', {
title: 'Network Throughput',
valFmt: v => formatBps(v),
series: [
{ label: 'Rx (In)', stroke: '#16a34a', fill: 'rgba(22, 163, 74, 0.1)' },
{ label: 'Tx (Out)', stroke: '#2563eb', fill: 'rgba(37, 99, 235, 0.1)' }
]
});
}
const diskIOEl = document.getElementById('chart-disk-io');
if (diskIOEl) {
C.mountChart(diskIOEl, 'chart-disk-io', {
title: 'Disk I/O Throughput',
valFmt: v => formatBps(v),
series: [
{ label: 'Read', stroke: '#0284c7' },
{ label: 'Write', stroke: '#dc2626' }
]
});
}
const diskSpaceEl = document.getElementById('chart-disk-space');
if (diskSpaceEl) {
C.mountChart(diskSpaceEl, 'chart-disk-space', {
title: 'Disk Usage (%)',
range: [0, 100],
valFmt: v => v.toFixed(1) + '%',
series: [{ label: 'Root (/) %', stroke: '#d97706', fill: 'rgba(217, 119, 6, 0.1)' }]
});
}
},
async refresh() {
if (!this.store.authenticated) return;
try {
const summaryData = await this.store.apiFetch('/api/v1/metrics/daily/summary');
if (summaryData && summaryData.host) {
this.summary = summaryData.host;
this.store.dailySummary = summaryData;
}
const histData = await this.store.apiFetch('/api/v1/metrics/daily?entity=host&days=14&include_today=true');
if (histData && histData.history) {
this.history = histData.history;
}
const spanSec = parseTimeRangeToSeconds(this.store.timeRange);
const nowSec = Math.floor(Date.now() / 1000);
const fromSec = nowSec - spanSec;
const fetchMetric = async (metric, entity = 'host') => {
try {
const res = await this.store.apiFetch(`/api/v1/host/metrics?entity=${encodeURIComponent(entity)}&metric=${encodeURIComponent(metric)}&from=${fromSec}&to=${nowSec}`);
return (res && res.points) || [];
} catch (e) {
return [];
}
};
const [cpuPts, l1Pts, l5Pts, l15Pts, memUsedPts, memAvailPts, swapUsedPts, netRxPts, netTxPts, diskReadPts, diskWritePts, diskUsedPctPts, diskTotalPts, diskUsedPts] = await Promise.all([
fetchMetric('cpu_pct'),
fetchMetric('load_1m'),
fetchMetric('load_5m'),
fetchMetric('load_15m'),
fetchMetric('mem_used'),
fetchMetric('mem_available'),
fetchMetric('swap_used'),
fetchMetric('net_rx_bps'),
fetchMetric('net_tx_bps'),
fetchMetric('disk_read_bps'),
fetchMetric('disk_write_bps'),
fetchMetric('disk_used_pct', 'disk:/'),
fetchMetric('disk_total_bytes', 'disk:/'),
fetchMetric('disk_used_bytes', 'disk:/')
]);
if (cpuPts.length > 0) this.current.cpuPct = cpuPts[cpuPts.length - 1].v.toFixed(1) + '%';
if (memUsedPts.length > 0) this.current.memUsed = formatBytes(memUsedPts[memUsedPts.length - 1].v);
if (memAvailPts.length > 0) this.current.memAvail = formatBytes(memAvailPts[memAvailPts.length - 1].v);
if (l1Pts.length > 0) this.current.load1 = l1Pts[l1Pts.length - 1].v.toFixed(2);
if (l5Pts.length > 0) this.current.load5 = l5Pts[l5Pts.length - 1].v.toFixed(2);
if (l15Pts.length > 0) this.current.load15 = l15Pts[l15Pts.length - 1].v.toFixed(2);
if (diskTotalPts.length > 0) this.current.diskTotal = formatBytes(diskTotalPts[diskTotalPts.length - 1].v);
if (diskUsedPts.length > 0) this.current.diskUsed = formatBytes(diskUsedPts[diskUsedPts.length - 1].v);
if (diskUsedPctPts.length > 0) this.current.diskUsedPct = diskUsedPctPts[diskUsedPctPts.length - 1].v.toFixed(1) + '%';
const C = window.TarsierCharts;
C.updateChart('chart-cpu', alignChartData([cpuPts]));
C.updateChart('chart-load', alignChartData([l1Pts, l5Pts, l15Pts]));
C.updateChart('chart-mem', alignChartData([memUsedPts, memAvailPts, swapUsedPts]));
C.updateChart('chart-net', alignChartData([netRxPts, netTxPts]));
C.updateChart('chart-disk-io', alignChartData([diskReadPts, diskWritePts]));
C.updateChart('chart-disk-space', alignChartData([diskUsedPctPts]));
} catch (e) {
}
},
startRefreshTimer() {
this.stopRefreshTimer();
const intervalStr = this.store.refreshInterval;
let ms = 10000;
if (intervalStr === '5s') ms = 5000;
else if (intervalStr === '10s') ms = 10000;
else if (intervalStr === '30s') ms = 30000;
else if (intervalStr === '0') return;
this._timer = setInterval(() => {
if (!document.hidden && (this.store.route === 'host' || this.store.route === 'overview')) {
this.refresh();
}
}, ms);
},
stopRefreshTimer() {
if (this._timer) {
clearInterval(this._timer);
this._timer = null;
}
}
};
}
window.HostController = HostController;
})();