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
function formatUptime(seconds) {
if (seconds == null || isNaN(seconds) || seconds <= 0) return '--';
const d = Math.floor(seconds / 86400);
const h = Math.floor((seconds % 86400) / 3600);
const m = Math.floor((seconds % 3600) / 60);
if (d > 0) return d + 'd ' + h + 'h';
if (h > 0) return h + 'h ' + m + 'm';
return m + 'm';
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
function ContainersController(appStore) {
let refreshTimer = null;
const detailCharts = {};
function destroyDetailCharts() {
Object.keys(detailCharts).forEach(key => {
if (detailCharts[key]) {
detailCharts[key].destroy();
delete detailCharts[key];
}
});
}
const ctrl = {
containers: [],
activeTab: 'running',
get runningContainers() {
return (this.containers || []).filter(c => c.state === 'running');
},
get exitedContainers() {
return (this.containers || []).filter(c => c.state !== 'running');
},
dockerConnected: true,
search: '',
sortCol: 'name',
sortAsc: true,
detailModalOpen: false,
detailLoading: false,
detailData: null,
detailContainerName: '',
getHealthCircleClass(c) {
if (!c) return 'circle-na';
const h = (c.health || '').toLowerCase();
const s = (c.state || '').toLowerCase();
if (h === 'green' || h === 'healthy') return 'circle-healthy';
if (h === 'red' || h === 'unhealthy' || s === 'exited' || s === 'dead' || s === 'restarting') return 'circle-unhealthy';
if (h === 'amber') return 'circle-unhealthy';
if (h === 'na' || h === 'n/a') return 'circle-na';
return s === 'running' ? 'circle-healthy' : 'circle-na';
},
mounted() {
this.refresh();
this.startRefreshTimer();
},
unmounted() {
this.stopRefreshTimer();
destroyDetailCharts();
},
detailMounted() {
this.startRefreshTimer();
const cName = (appStore.routeParams && appStore.routeParams.container) || (new URLSearchParams(window.location.hash.split('?')[1] || '').get('container')) || this.detailContainerName;
if (cName) {
this.loadDetail(cName);
}
},
detailUnmounted() {
destroyDetailCharts();
this.detailData = null;
},
startRefreshTimer() {
if (refreshTimer) clearInterval(refreshTimer);
const intervalMap = { '5s': 5000, '10s': 10000, '30s': 30000 };
const ms = intervalMap[appStore.refreshInterval];
if (ms) {
refreshTimer = setInterval(() => {
if (appStore.route === 'containers' || appStore.route === 'overview') {
this.refresh();
} else if (appStore.route === 'container-detail' && this.detailContainerName) {
this.loadDetail(this.detailContainerName, true);
}
}, ms);
}
},
stopRefreshTimer() {
if (refreshTimer) {
clearInterval(refreshTimer);
refreshTimer = null;
}
},
async refresh() {
try {
const res = await appStore.apiFetch('/api/v1/containers');
if (res) {
this.containers = res.containers || [];
this.dockerConnected = res.docker_connected !== false;
}
} catch (e) {
}
},
setSort(col) {
if (this.sortCol === col) {
this.sortAsc = !this.sortAsc;
} else {
this.sortCol = col;
this.sortAsc = true;
}
},
get filteredContainers() {
const base = this.activeTab === 'exited' ? this.exitedContainers : this.runningContainers;
const q = (this.search || '').trim().toLowerCase();
let list = base.slice();
if (q) {
list = list.filter(c =>
(c.name && c.name.toLowerCase().includes(q)) ||
(c.image && c.image.toLowerCase().includes(q)) ||
(c.state && c.state.toLowerCase().includes(q))
);
}
const col = this.sortCol;
const asc = this.sortAsc ? 1 : -1;
list.sort((a, b) => {
let va, vb;
switch (col) {
case 'name':
va = (a.name || '').toLowerCase();
vb = (b.name || '').toLowerCase();
return va.localeCompare(vb) * asc;
case 'state':
va = (a.state || '').toLowerCase();
vb = (b.state || '').toLowerCase();
return va.localeCompare(vb) * asc;
case 'cpu':
va = (a.metrics && a.metrics.cpu_pct) || 0;
vb = (b.metrics && b.metrics.cpu_pct) || 0;
return (va - vb) * asc;
case 'mem':
va = (a.metrics && a.metrics.mem_used) || 0;
vb = (b.metrics && b.metrics.mem_used) || 0;
return (va - vb) * asc;
case 'restarts':
va = a.restart_count || 0;
vb = b.restart_count || 0;
return (va - vb) * asc;
default:
return 0;
}
});
return list;
},
openDetail(name) {
this.detailContainerName = name;
this.loadDetail(name);
appStore.setRoute('container-detail', { container: name });
},
async loadDetail(name, silent = false) {
if (!name) return;
this.detailContainerName = name;
if (!silent) {
this.detailLoading = true;
destroyDetailCharts();
}
try {
const data = await appStore.apiFetch('/api/v1/containers/' + encodeURIComponent(name) + '?days=7');
this.detailData = data;
this.detailLoading = false;
setTimeout(() => {
this.renderDetailCharts(name);
}, 50);
} catch (e) {
this.detailLoading = false;
}
},
closeDetail() {
this.detailModalOpen = false;
this.detailData = null;
destroyDetailCharts();
appStore.setRoute('containers');
},
viewLogs(name) {
this.closeDetail();
appStore.selectedLogSource = 'container:' + name;
appStore.setRoute('logs', { container: name });
},
async renderDetailCharts(name) {
if (!window.uPlot) return;
destroyDetailCharts();
const entity = 'container:' + name;
const now = Math.floor(Date.now() / 1000);
const from = now - parseTimeRangeToSeconds(appStore.timeRange);
const fetchMetric = async (metric) => {
try {
const res = await appStore.apiFetch('/api/v1/host/metrics?entity=' + encodeURIComponent(entity) + '&metric=' + metric + '&from=' + from + '&to=' + now + '&step=auto');
return (res && res.points) || [];
} catch (e) {
return [];
}
};
const [cpuPts, memPts, rxPts, txPts] = await Promise.all([
fetchMetric('cpu_pct'),
fetchMetric('mem_used'),
fetchMetric('net_rx_bps'),
fetchMetric('net_tx_bps')
]);
const theme = window.ChartColors ? window.ChartColors.getTheme() : { text: '#475569', grid: '#e2e8f0' };
const cpuEl = document.getElementById('detail-chart-cpu') || document.getElementById('modal-chart-cpu');
if (cpuEl) {
const cpuData = alignChartData([cpuPts]);
detailCharts.cpu = new window.uPlot({
title: 'CPU Utilization (%)',
width: cpuEl.clientWidth || 380,
height: 180,
series: [
{},
{ label: 'CPU %', stroke: '#2563eb', width: 1.5, fill: 'rgba(37, 99, 235, 0.1)', value: (u, v) => (v != null ? v.toFixed(1) + '%' : '--') }
],
axes: [
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 } },
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 }, values: (u, vals) => vals.map(v => (v != null ? v.toFixed(0) + '%' : '')) }
]
}, cpuData, cpuEl);
}
const memEl = document.getElementById('detail-chart-mem') || document.getElementById('modal-chart-mem');
if (memEl) {
const memData = alignChartData([memPts]);
detailCharts.mem = new window.uPlot({
title: 'Memory Usage',
width: memEl.clientWidth || 380,
height: 180,
series: [
{},
{ label: 'RAM', stroke: '#10b981', width: 1.5, fill: 'rgba(16, 185, 129, 0.1)', value: (u, v) => (v != null ? formatBytes(v) : '--') }
],
axes: [
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 } },
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 }, values: (u, vals) => vals.map(v => (v != null ? formatBytes(v) : '')) }
]
}, memData, memEl);
}
const netEl = document.getElementById('detail-chart-net') || document.getElementById('modal-chart-net');
if (netEl) {
const netData = alignChartData([rxPts, txPts]);
detailCharts.net = new window.uPlot({
title: 'Network Throughput (RX / TX)',
width: netEl.clientWidth || 780,
height: 180,
series: [
{},
{ label: 'Net RX', stroke: '#8b5cf6', width: 1.5, value: (u, v) => (v != null ? formatBps(v) : '--') },
{ label: 'Net TX', stroke: '#06b6d4', width: 1.5, value: (u, v) => (v != null ? formatBps(v) : '--') }
],
axes: [
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 } },
{ stroke: theme.text, grid: { stroke: theme.grid, width: 1 }, values: (u, vals) => vals.map(v => (v != null ? formatBps(v) : '')) }
]
}, netData, netEl);
}
},
formatBytes,
formatBps,
formatUptime
};
return window.PetiteVue ? window.PetiteVue.reactive(ctrl) : ctrl;
}
window.ContainersController = ContainersController;
})();