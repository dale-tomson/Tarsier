(function() {
'use strict';
function formatTime(isoStr) {
if (!isoStr) return '--';
try {
const d = new Date(isoStr);
if (isNaN(d.getTime())) return isoStr;
const pad = (n, len = 2) => String(n).padStart(len, '0');
const y = d.getUTCFullYear();
const m = pad(d.getUTCMonth() + 1);
const day = pad(d.getUTCDate());
const hh = pad(d.getUTCHours());
const mm = pad(d.getUTCMinutes());
const ss = pad(d.getUTCSeconds());
const ms = pad(d.getUTCMilliseconds(), 3);
return `${y}-${m}-${day} ${hh}:${mm}:${ss}.${ms}`;
} catch (e) {
return isoStr;
}
}
function highlightSegments(line, query, isRegex) {
if (!query || !line) {
return [{ text: line, match: false }];
}
if (line.length > 10000) {
return [{ text: line, match: false }];
}
let re;
if (isRegex) {
try {
re = new RegExp(query, 'gi');
} catch (e) {
return [{ text: line, match: false }];
}
} else {
const escaped = query.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
re = new RegExp(escaped, 'gi');
}
const segments = [];
let lastIndex = 0;
let match;
while ((match = re.exec(line)) !== null) {
if (match.index > lastIndex) {
segments.push({ text: line.substring(lastIndex, match.index), match: false });
}
segments.push({ text: match[0], match: true });
lastIndex = re.lastIndex;
if (match[0].length === 0) {
re.lastIndex++;
}
}
if (lastIndex < line.length) {
segments.push({ text: line.substring(lastIndex), match: false });
}
return segments.length > 0 ? segments : [{ text: line, match: false }];
}
function sortLatestFirst(entries) {
if (!Array.isArray(entries)) return [];
return entries.sort((a, b) => {
const tsA = (a && a.ts) || '';
const tsB = (b && b.ts) || '';
if (tsA !== tsB) {
return tsA < tsB ? 1 : -1;
}
const idA = (a && a.id) || '';
const idB = (b && b.id) || '';
return idA < idB ? 1 : (idA > idB ? -1 : 0);
});
}
window.LogsController = function(appStore) {
return {
sources: [],
selectedSource: '',
sourceDropdownOpen: false,
sourceSearchQuery: '',
highlightedSourceIndex: 0,
boundDocClickHandler: null,
query: '',
regex: false,
level: '',
stream: '',
limit: 100,
entries: [],
loading: false,
error: null,
nextCursor: '',
hasMore: false,
scannedBlocks: 0,
fromCache: false,
polling: true,
pollTimer: null,
pollInterval: 3000,
pollInflight: false,
newLogsCount: 0,
newLogsTimer: null,
isFullscreen: false,
wrapLines: true,
boundKeyHandler: null,
expandedID: null,
contextModalOpen: false,
contextLoading: false,
contextEntries: [],
contextTargetIndex: -1,
contextSource: '',
get currentSource() {
return (this.sources || []).find(s => String(s.id) === String(this.selectedSource)) || null;
},
get filteredSources() {
const list = this.sources || [];
if (!this.sourceSearchQuery || !this.sourceSearchQuery.trim()) {
return list;
}
const q = this.sourceSearchQuery.trim().toLowerCase();
return list.filter(s => {
const name = (s.name || '').toLowerCase();
const kind = (s.kind || '').toLowerCase();
const typeLabel = (this.getSourceTypeLabel(s) || '').toLowerCase();
return name.includes(q) || kind.includes(q) || typeLabel.includes(q);
});
},
getSourceTypeLabel(s) {
if (!s) return '';
if (s.kind === 'container') return 'container';
if (s.kind === 'file') {
if (s.name && (s.name.startsWith('/var/log') || s.name.startsWith('var/log'))) {
return 'var log';
}
return 'file';
}
return s.kind || 'log';
},
isContainer(s) {
return s && s.kind === 'container';
},
isContainerLive(s) {
return s && s.kind === 'container' && (s.status === 'running' || s.status === 'healthy');
},
toggleSourceDropdown() {
if (this.sourceDropdownOpen) {
this.closeSourceDropdown();
} else {
this.openSourceDropdown();
}
},
openSourceDropdown() {
this.sourceDropdownOpen = true;
this.sourceSearchQuery = '';
this.highlightedSourceIndex = 0;
setTimeout(() => {
const el = document.getElementById('log-source-search-input');
if (el) el.focus();
}, 50);
},
closeSourceDropdown() {
this.sourceDropdownOpen = false;
this.sourceSearchQuery = '';
this.highlightedSourceIndex = 0;
},
selectSource(s) {
if (!s) return;
this.selectedSource = String(s.id);
this.closeSourceDropdown();
this.onSourceChange();
},
navigateSource(step) {
const list = this.filteredSources;
if (!list || list.length === 0) return;
let next = this.highlightedSourceIndex + step;
if (next < 0) next = 0;
if (next >= list.length) next = list.length - 1;
this.highlightedSourceIndex = next;
setTimeout(() => {
const activeEl = document.querySelector('.custom-select-option.highlighted');
if (activeEl && activeEl.scrollIntoView) {
activeEl.scrollIntoView({ block: 'nearest' });
}
}, 0);
},
selectHighlightedSource() {
const list = this.filteredSources;
if (list && list.length > 0) {
const idx = Math.max(0, Math.min(this.highlightedSourceIndex, list.length - 1));
this.selectSource(list[idx]);
}
},
mounted() {
this.loadSources();
if (this.polling) {
this.startPolling();
}
this.boundKeyHandler = (e) => {
if (e.key === 'Escape') {
if (this.sourceDropdownOpen) {
this.closeSourceDropdown();
} else if (this.isFullscreen) {
this.toggleFullscreen();
}
}
};
window.addEventListener('keydown', this.boundKeyHandler);

this.boundDocClickHandler = (e) => {
if (!this.sourceDropdownOpen) return;
const selectWrap = document.getElementById('log-source-select-wrap');
if (selectWrap && !selectWrap.contains(e.target)) {
this.closeSourceDropdown();
}
};
document.addEventListener('click', this.boundDocClickHandler);
},
unmounted() {
this.stopPolling();
if (this.boundKeyHandler) {
window.removeEventListener('keydown', this.boundKeyHandler);
this.boundKeyHandler = null;
}
if (this.boundDocClickHandler) {
document.removeEventListener('click', this.boundDocClickHandler);
this.boundDocClickHandler = null;
}
if (this.newLogsTimer) {
clearTimeout(this.newLogsTimer);
this.newLogsTimer = null;
}
},
async loadSources() {
try {
const hashQuery = window.location.hash.includes('?') ? window.location.hash.split('?')[1] : '';
const urlParams = new URLSearchParams(hashQuery);
let containerParam = (appStore.routeParams && appStore.routeParams.container) || urlParams.get('container') || '';
if (containerParam) {
containerParam = decodeURIComponent(containerParam).trim().replace(/^\/+/, '');
}
const res = await appStore.apiFetch('/api/v1/sources');
this.sources = (res && res.sources) || [];
let target = appStore.selectedLogSource || (containerParam ? ('container:' + containerParam) : '');
appStore.selectedLogSource = null;
if (target) {
const cleanTarget = target.replace(/^container:\/?/, '');
const matched = this.sources.find(s =>
String(s.id) === target ||
s.name === target ||
(s.kind + ':' + s.name) === target ||
s.name === cleanTarget ||
(s.kind === 'container' && s.name === cleanTarget)
);
if (matched) {
this.selectedSource = String(matched.id);
this.error = null;
} else {
this.selectedSource = '';
this.entries = [];
this.error = 'Source "' + target + '" not found. It may not have generated logs yet.';
return;
}
} else if (!this.selectedSource || !this.sources.some(s => String(s.id) === this.selectedSource)) {
if (this.sources.length > 0) {
this.selectedSource = String(this.sources[0].id);
this.error = null;
} else {
this.selectedSource = '';
this.entries = [];
}
}
if (this.selectedSource) {
this.fetchLogs(false);
}
} catch (e) {
this.error = e.message;
}
},
async refreshAll() {
await this.loadSources();
},
onSourceChange() {
this.nextCursor = '';
this.hasMore = false;
this.entries = [];
this.error = null;
this.fetchLogs(false);
},
onFilterChange() {
this.nextCursor = '';
this.hasMore = false;
this.fetchLogs(false);
},
toggleFullscreen() {
this.isFullscreen = !this.isFullscreen;
},
toggleWrap() {
this.wrapLines = !this.wrapLines;
},
clearQuery() {
this.query = '';
this.onFilterChange();
},
resetFilters() {
this.query = '';
this.regex = false;
this.level = '';
this.stream = '';
this.limit = 100;
this.onFilterChange();
},
toggleRegex() {
this.regex = !this.regex;
this.onFilterChange();
},
togglePolling() {
this.polling = !this.polling;
if (this.polling) {
this.startPolling();
} else {
this.stopPolling();
}
},
startPolling() {
this.stopPolling();
this.pollTimer = setInterval(() => {
if (!document.hidden && appStore.authenticated && appStore.route === 'logs') {
this.pollLatest();
}
}, this.pollInterval);
},
stopPolling() {
if (this.pollTimer) {
clearInterval(this.pollTimer);
this.pollTimer = null;
}
},
async fetchLogs(appendMode = false) {
if (!this.selectedSource) return;
this.loading = true;
this.error = null;
try {
const limitVal = this.limit || 100;
let url = '/api/v1/logs?source=' + encodeURIComponent(this.selectedSource) + '&limit=' + encodeURIComponent(limitVal);
if (this.query) {
url += '&q=' + encodeURIComponent(this.query);
if (this.regex) {
url += '&regex=true';
}
}
if (this.level) {
url += '&level=' + encodeURIComponent(this.level);
}
if (this.stream) {
url += '&stream=' + encodeURIComponent(this.stream);
}
if (appendMode && this.nextCursor) {
url += '&cursor=' + encodeURIComponent(this.nextCursor);
}
const res = await appStore.apiFetch(url);
const newEntries = (res && res.entries) || [];
this.hasMore = !!(res && res.hasMore);
this.nextCursor = (res && res.nextCursor) || '';
this.scannedBlocks = (res && res.scannedBlocks) || 0;
this.fromCache = !!(res && res.fromCache);
if (appendMode) {
const seen = new Set(this.entries.map(e => e.id));
for (let i = 0; i < newEntries.length; i++) {
if (!seen.has(newEntries[i].id)) {
this.entries.push(newEntries[i]);
seen.add(newEntries[i].id);
}
}
sortLatestFirst(this.entries);
} else {
this.entries = sortLatestFirst(newEntries);
}
} catch (e) {
this.error = e.message;
} finally {
this.loading = false;
}
},
async pollLatest() {
if (!this.selectedSource || this.loading || this.pollInflight) return;
this.pollInflight = true;
try {
let url = '/api/v1/logs?source=' + encodeURIComponent(this.selectedSource) + '&limit=50';
if (this.query) {
url += '&q=' + encodeURIComponent(this.query);
if (this.regex) url += '&regex=true';
}
if (this.level) url += '&level=' + encodeURIComponent(this.level);
if (this.stream) url += '&stream=' + encodeURIComponent(this.stream);
const res = await appStore.apiFetch(url);
const latestEntries = (res && res.entries) || [];
if (latestEntries.length === 0) return;
if (this.entries.length === 0) {
this.entries = sortLatestFirst(latestEntries);
this.nextCursor = (res && res.nextCursor) || '';
this.hasMore = !!(res && res.hasMore);
return;
}
const existingIDs = new Set(this.entries.map(e => e.id));
const toAdd = [];
for (let i = 0; i < latestEntries.length; i++) {
if (!existingIDs.has(latestEntries[i].id)) {
toAdd.push(latestEntries[i]);
existingIDs.add(latestEntries[i].id);
}
}
if (toAdd.length > 0) {
this.entries = sortLatestFirst(toAdd.concat(this.entries));
if (this.entries.length > 2000) {
this.entries = this.entries.slice(0, 2000);
}
this.newLogsCount = toAdd.length;
if (this.newLogsTimer) clearTimeout(this.newLogsTimer);
this.newLogsTimer = setTimeout(() => {
this.newLogsCount = 0;
this.newLogsTimer = null;
}, 2500);
}
} catch (e) {
} finally {
this.pollInflight = false;
}
},
loadOlder() {
if (this.hasMore && !this.loading) {
this.fetchLogs(true);
}
},
toggleExpand(id) {
this.expandedID = this.expandedID === id ? null : id;
},
async openContext(entry) {
if (!entry || !entry.id) return;
this.contextModalOpen = true;
this.contextLoading = true;
this.contextEntries = [];
this.contextTargetIndex = -1;
this.contextSource = this.selectedSource;
try {
const url = '/api/v1/logs/context?source=' + encodeURIComponent(this.selectedSource) +
'&id=' + encodeURIComponent(entry.id) + '&before=20&after=20';
const res = await appStore.apiFetch(url);
this.contextEntries = (res && res.entries) || [];
this.contextTargetIndex = (res && res.target_index !== undefined) ? res.target_index : -1;
} catch (e) {
appStore.toast.show('Failed to load log context: ' + e.message, 'error');
this.contextModalOpen = false;
} finally {
this.contextLoading = false;
}
},
closeContext() {
this.contextModalOpen = false;
this.contextEntries = [];
this.contextTargetIndex = -1;
},
copyLine(text) {
if (navigator.clipboard && navigator.clipboard.writeText) {
navigator.clipboard.writeText(text).then(() => {
appStore.toast.show('Line copied to clipboard', 'info', 2000);
}).catch(() => {});
}
},
formatTime,
highlightSegments(line) {
return highlightSegments(line, this.query, this.regex);
},
levelBadgeClass(level) {
switch ((level || '').toLowerCase()) {
case 'fatal': return 'badge-fatal';
case 'error': return 'badge-danger';
case 'warn':
case 'warning': return 'badge-warning';
case 'info': return 'badge-success';
case 'debug': return 'badge-muted';
default: return 'badge-muted';
}
}
};
};
})();