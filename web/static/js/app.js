(function() {
'use strict';
const VALID_ROUTES = ['overview', 'host', 'containers', 'container-detail', 'logs', 'alerts', 'settings'];
const savedTheme = localStorage.getItem('tarsier-theme') || 'auto';
applyTheme(savedTheme);
function applyTheme(theme) {
if (theme === 'auto') {
const isDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
document.documentElement.setAttribute('data-theme', isDark ? 'dark' : 'light');
} else {
document.documentElement.setAttribute('data-theme', theme);
}
}
function parseHash() {
const raw = window.location.hash.replace(/^#\/?/, '').trim();
const qIdx = raw.indexOf('?');
const path = (qIdx !== -1 ? raw.slice(0, qIdx) : raw).toLowerCase();
const queryStr = qIdx !== -1 ? raw.slice(qIdx + 1) : '';
const searchParams = new URLSearchParams(queryStr);
const params = {};
for (const [k, v] of searchParams.entries()) {
params[k] = v;
}
return {
route: VALID_ROUTES.includes(path) ? path : 'overview',
params: params
};
}
function getRouteFromHash() {
return parseHash().route;
}
const toast = window.ToastState ? window.ToastState() : { show: () => {}, dismiss: () => {} };
const store = window.PetiteVue.reactive({
route: getRouteFromHash(),
routeParams: parseHash().params,
setRoute(r, params) {
if (VALID_ROUTES.includes(r)) {
this.route = r;
const pObj = (params && typeof params === 'object') ? params : {};
this.routeParams = pObj;
let hash = '#' + r;
const q = new URLSearchParams(pObj).toString();
if (q) hash += '?' + q;
window.location.hash = hash;
if (r === 'settings' && this.authenticated) {
this.loadSettings();
} else if (r === 'container-detail' && pObj.container) {
if (window.containersView && window.containersView.loadDetail) {
window.containersView.loadDetail(pObj.container);
}
} else if (r === 'containers' || r === 'overview') {
if (window.containersView && window.containersView.refresh) {
window.containersView.refresh();
}
} else if (r === 'logs') {
if (pObj && pObj.container) {
this.selectedLogSource = 'container:' + pObj.container;
}
}
}
},
theme: savedTheme,
cycleTheme() {
const order = ['auto', 'light', 'dark'];
const nextIdx = (order.indexOf(this.theme) + 1) % order.length;
this.theme = order[nextIdx];
localStorage.setItem('tarsier-theme', this.theme);
applyTheme(this.theme);
toast.show('Theme set to ' + this.theme, 'info', 2000);
},
timeRange: '1h',
refreshInterval: '10s',
onTimeRangeChange() {
if (window.hostView && (this.route === 'host' || this.route === 'overview')) {
window.hostView.refresh();
}
if (window.containersView && (this.route === 'container-detail' || (this.route === 'containers' && window.containersView.detailModalOpen)) && window.containersView.detailData) {
window.containersView.renderDetailCharts(window.containersView.detailData.container.name);
}
if (window.logsView && this.route === 'logs') {
window.logsView.timeRange = this.timeRange;
window.logsView.onFilterChange();
}
},
onRefreshIntervalChange() {
if (window.hostView && window.hostView.startRefreshTimer) {
window.hostView.startRefreshTimer();
}
if (window.containersView && window.containersView.startRefreshTimer) {
window.containersView.startRefreshTimer();
}
if (window.alertsView && window.alertsView.startTimer) {
window.alertsView.startTimer();
}
},
dailySummary: null,
authenticated: false,
user: '',
loginForm: {
username: '',
password: '',
error: '',
loading: false
},
toast: toast,
systemConfig: null,
systemHealth: null,
async apiFetch(path, opts = {}) {
opts.headers = opts.headers || {};
if (opts.body && typeof opts.body === 'object' && !(opts.body instanceof FormData)) {
opts.headers['Content-Type'] = 'application/json';
opts.body = JSON.stringify(opts.body);
}
try {
const res = await fetch(path, opts);
if (res.status === 401) {
this.authenticated = false;
this.user = '';
if (path !== '/api/v1/auth/me') {
this.toast.show('Session expired. Please log in again.', 'warning');
}
throw new Error('Unauthorized');
}
const data = await res.json().catch(() => ({}));
if (!res.ok) {
const errMsg = (data && data.error && data.error.message) || ('HTTP ' + res.status);
throw new Error(errMsg);
}
return data;
} catch (err) {
if (err.message !== 'Unauthorized') {
this.toast.show(err.message, 'error');
}
throw err;
}
},
async checkAuth() {
try {
const data = await this.apiFetch('/api/v1/auth/me');
this.authenticated = true;
this.user = data.username || 'admin';
if (this.route === 'overview') {
this.onOverviewMounted();
} else if (this.route === 'settings') {
this.loadSettings();
} else if (this.route === 'container-detail') {
const cName = this.routeParams ? (this.routeParams.container || '') : '';
if (window.containersView && window.containersView.loadDetail) {
window.containersView.loadDetail(cName);
}
} else if (this.route === 'logs') {
const cName = this.routeParams ? (this.routeParams.container || '') : '';
if (cName) this.selectedLogSource = 'container:' + cName;
}
} catch (e) {
this.authenticated = false;
this.user = '';
}
},
async login() {
if (!this.loginForm.username || !this.loginForm.password) {
this.loginForm.error = 'Username and password required';
return;
}
this.loginForm.loading = true;
this.loginForm.error = '';
try {
const res = await fetch('/api/v1/auth/login', {
method: 'POST',
headers: { 'Content-Type': 'application/json' },
body: JSON.stringify({
username: this.loginForm.username,
password: this.loginForm.password
})
});
const data = await res.json().catch(() => ({}));
if (!res.ok) {
const msg = (data && data.error && data.error.message) || 'Login failed';
this.loginForm.error = msg;
this.toast.show(msg, 'error');
return;
}
this.authenticated = true;
this.user = data.username || this.loginForm.username;
this.loginForm.password = '';
this.loginForm.error = '';
this.toast.show('Logged in successfully', 'success');
if (this.route === 'overview') {
this.onOverviewMounted();
} else if (this.route === 'settings') {
this.loadSettings();
} else if (this.route === 'container-detail') {
const cName = this.routeParams ? (this.routeParams.container || '') : '';
if (window.containersView && window.containersView.loadDetail) {
window.containersView.loadDetail(cName);
}
} else if (this.route === 'logs') {
const cName = this.routeParams ? (this.routeParams.container || '') : '';
if (cName) this.selectedLogSource = 'container:' + cName;
}
} catch (err) {
this.loginForm.error = err.message || 'Network error';
this.toast.show(this.loginForm.error, 'error');
} finally {
this.loginForm.loading = false;
}
},
async logout() {
try {
await this.apiFetch('/api/v1/auth/logout', { method: 'POST' });
} catch (e) {
}
this.authenticated = false;
this.user = '';
this.toast.show('Logged out', 'info');
},
async loadSettings() {
try {
const [cfg, health] = await Promise.all([
this.apiFetch('/api/v1/system/config'),
this.apiFetch('/api/v1/system/health')
]);
this.systemConfig = cfg;
this.systemHealth = health;
if (window.settingsView && window.settingsView.mounted) {
window.settingsView.mounted();
}
} catch (e) {
}
},
async loadHealth() {
try {
this.systemHealth = await this.apiFetch('/api/v1/system/health');
} catch (e) {
}
},
onOverviewMounted() {
if (!this.authenticated) return;
if (window.hostView) {
if (window.hostView.refresh) window.hostView.refresh();
if (window.hostView.startRefreshTimer) window.hostView.startRefreshTimer();
}
if (window.containersView) {
if (window.containersView.refresh) window.containersView.refresh();
if (window.containersView.startRefreshTimer) window.containersView.startRefreshTimer();
}
if (window.alertsView) {
if (window.alertsView.fetchActive) window.alertsView.fetchActive();
if (window.alertsView.startTimer) window.alertsView.startTimer();
}
this.loadHealth();
},
onOverviewUnmounted() {
if (window.hostView && window.hostView.stopRefreshTimer) {
window.hostView.stopRefreshTimer();
}
if (window.containersView && window.containersView.stopRefreshTimer) {
window.containersView.stopRefreshTimer();
}
}
});
window.addEventListener('hashchange', () => {
const parsed = parseHash();
store.route = parsed.route;
store.routeParams = parsed.params;
if (store.authenticated) {
if (store.route === 'settings') {
store.loadSettings();
if (window.settingsView && window.settingsView.mounted) {
window.settingsView.mounted();
}
} else if (store.route === 'containers' || store.route === 'overview') {
if (window.containersView && window.containersView.refresh) {
window.containersView.refresh();
}
} else if (store.route === 'container-detail') {
const cName = parsed.params.container || '';
if (window.containersView && window.containersView.loadDetail) {
window.containersView.loadDetail(cName);
}
} else if (store.route === 'logs') {
const cName = parsed.params.container;
if (cName) {
store.selectedLogSource = 'container:' + cName;
}
if (window.logsView && window.logsView.mounted) {
window.logsView.mounted();
}
} else if (store.route === 'alerts') {
if (window.alertsView && window.alertsView.mounted) {
window.alertsView.mounted();
}
}
}
});
if (window.matchMedia) {
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
if (store.theme === 'auto') applyTheme('auto');
});
}
window.store = store;
const hostView = window.PetiteVue.reactive(window.HostController ? window.HostController(store) : {});
window.hostView = hostView;
const containersView = window.PetiteVue.reactive(window.ContainersController ? window.ContainersController(store) : {});
window.containersView = containersView;
const logsView = window.PetiteVue.reactive(window.LogsController ? window.LogsController(store) : {});
window.logsView = logsView;
const alertsView = window.PetiteVue.reactive(window.AlertsController ? window.AlertsController(store) : {});
window.alertsView = alertsView;
const settingsView = window.PetiteVue.reactive(window.SettingsController ? window.SettingsController(store) : {});
window.settingsView = settingsView;
window.PetiteVue.createApp({
store,
hostView,
containersView,
logsView,
alertsView,
settingsView,
overviewChart: window.ChartPanel ? window.ChartPanel('overview-chart') : {}
}).mount();
document.addEventListener('visibilitychange', () => {
if (!document.hidden && store.authenticated) {
if (store.route === 'host' || store.route === 'overview') {
hostView.refresh();
}
if (store.route === 'containers' || store.route === 'overview') {
containersView.refresh();
}
if (store.route === 'logs') {
logsView.pollLatest();
}
if (store.route === 'alerts' || store.route === 'overview') {
alertsView.fetchActive();
}
}
});
store.checkAuth();
})();