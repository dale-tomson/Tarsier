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
window.LogsController = function(appStore) {
return {
sources: [],
selectedSource: '',
query: '',
regex: false,
level: '',
stream: '',
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
expandedID: null,
contextModalOpen: false,
contextLoading: false,
contextEntries: [],
contextTargetIndex: -1,
contextSource: '',
mounted() {
this.loadSources();
if (this.polling) {
this.startPolling();
}
},
unmounted() {
this.stopPolling();
},
async loadSources() {
try {
const params = appStore.routeParams || new URLSearchParams(window.location.hash.split('?')[1] || '');
const containerParam = params.get('container');
if (containerParam && !appStore.selectedLogSource) {
appStore.selectedLogSource = 'container:' + containerParam;
}
const res = await appStore.apiFetch('/api/v1/sources');
this.sources = (res && res.sources) || [];
if (appStore.selectedLogSource) {
const target = appStore.selectedLogSource;
appStore.selectedLogSource = null;
const matched = this.sources.find(s =>
s.name === target ||
(s.kind + ':' + s.name) === target ||
String(s.id) === target
);
if (matched) {
this.selectedSource = String(matched.id);
} else {
this.selectedSource = target;
}
} else if (!this.selectedSource && this.sources.length > 0) {
this.selectedSource = String(this.sources[0].id);
}
if (this.selectedSource) {
this.fetchLogs(false);
}
} catch (e) {
this.error = e.message;
}
},
onSourceChange() {
this.nextCursor = '';
this.hasMore = false;
this.entries = [];
this.fetchLogs(false);
},
onFilterChange() {
this.nextCursor = '';
this.hasMore = false;
this.fetchLogs(false);
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
let url = '/api/v1/logs?source=' + encodeURIComponent(this.selectedSource) + '&limit=100';
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
this.entries = this.entries.concat(newEntries);
} else {
this.entries = newEntries;
}
} catch (e) {
this.error = e.message;
} finally {
this.loading = false;
}
},
async pollLatest() {
if (!this.selectedSource || this.loading) return;
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
this.entries = latestEntries;
this.nextCursor = (res && res.nextCursor) || '';
this.hasMore = !!(res && res.hasMore);
return;
}
const existingIDs = new Set(this.entries.slice(0, 100).map(e => e.id));
const toPrepend = [];
for (let i = 0; i < latestEntries.length; i++) {
if (!existingIDs.has(latestEntries[i].id)) {
toPrepend.push(latestEntries[i]);
} else {
break;
}
}
if (toPrepend.length > 0) {
this.entries = toPrepend.concat(this.entries);
if (this.entries.length > 2000) {
this.entries = this.entries.slice(0, 2000);
}
}
} catch (e) {
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