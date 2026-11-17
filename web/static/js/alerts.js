(function() {
  'use strict';
  function fmtTime(v) {
    if(!v)return '--';
    try {
      const d=typeof v==='number' ? new Date(v * 1000):new Date(v);
      if(isNaN(d.getTime()))return String(v);
      const p=n=> String(n).padStart(2,'0');
      return `${d.getUTCFullYear()}-${p(d.getUTCMonth()+1)}-${p(d.getUTCDate())}${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`;
    } catch(_) {
      return String(v);
    }
  }
  function fmtDur(s) {
    if(s==null || isNaN(s)|| s <=0)return 'Instant';
    if(s < 60)return s + 's';
    if(s < 3600)return `${Math.floor(s/60)}m` +(s % 60 ? ` ${s%60}s`:'');
    return `${Math.floor(s/3600)}h` +(Math.floor((s%3600)/60)? ` ${Math.floor((s%3600)/60)}m`:'');
  }
  function defaultForm() {
    return {
      id:null,name:'',kind:'metric',target:'host',severity:'critical',for_sec:30,cooldown_sec:300,enabled:true,channels: {
        email:true,discord:false
      }
      ,metricName:'cpu_pct',metricOp:'>',metricThreshold:90.0,logPattern:'',logIsRegex:false,logThreshold:5,logWindowSec:60,logMinLevel:5,containerEvents: {
        oom:true,die:false,restart:false,stop:false,down:false
      }
      ,containerThreshold:1,containerWindowSec:0
    }
    ;
  }
  function AlertsController(store) {
    let timer=null;
    return {
      activeAlerts:[],rules:[],templates:[],historyEvents:[],availableContainers:[],channelStatus:{email:false,discord:false},ruleSearch:'',activeTab:'rules',historyLimit:25,historySeverity:'',historyState:'',historyHasMore:false,historyLoading:false,modalOpen:false,isEditing:false,modalLoading:false,ruleForm:defaultForm(),previewLoading:false,previewResult:null,mounted() {
        this.refresh();
        this.fetchTemplates();
        this.fetchContainers();
        this.fetchChannelStatus();
        this.startTimer();
      }
      ,unmounted() {
        if(timer) {
          clearInterval(timer);
          timer=null;
        }
      }
      ,startTimer() {
        if(timer)clearInterval(timer);
        const ms= {
          '5s':5000,'10s':10000,'30s':30000
        }
        [store.refreshInterval] || 10000;
        timer=setInterval(()=> {
          if(store.route==='alerts' || store.route==='overview') {
            this.fetchActive();
            if(store.route==='alerts' && this.activeTab==='rules')this.fetchRules();
          }
        }
        ,ms);
      }
      ,async refresh() {
        await Promise.all([this.fetchActive(),this.fetchRules(),this.fetchHistory(false)]);
      }
      ,async fetchActive() {
        try {
          const d=await store.apiFetch('/api/v1/alerts/active');
          this.activeAlerts=Array.isArray(d)? d:[];
        } catch(_) {
        }
      }
      ,async fetchRules() {
        try {
          const d=await store.apiFetch('/api/v1/alerts/rules');
          this.rules=Array.isArray(d)? d:[];
        } catch(_) {
        }
      }
      ,async fetchTemplates() {
        try {
          const d=await store.apiFetch('/api/v1/alerts/templates');
          this.templates=Array.isArray(d)? d:[];
        } catch(_) {
        }
      }
      ,async fetchHistory(append=false) {
        if(this.historyLoading)return;
        this.historyLoading=true;
        try {
          let u=`/api/v1/alerts/history?limit=${this.historyLimit}`;
          if(this.historySeverity)u +=`&severity=${encodeURIComponent(this.historySeverity)}`;
          if(this.historyState)u +=`&state=${encodeURIComponent(this.historyState)}`;
          if(append && this.historyEvents.length) {
            const last=this.historyEvents[this.historyEvents.length - 1];
            if(last && last.started)u +=`&cursor=${last.started}`;
          }
          const d=await store.apiFetch(u);
          const list=Array.isArray(d)? d:[];
          this.historyEvents=append ? this.historyEvents.concat(list):list;
          this.historyHasMore=list.length >=this.historyLimit;
        } catch(_) {
        } finally {
          this.historyLoading=false;
        }
      }
      ,filteredRules() {
        const q=(this.ruleSearch || '').trim().toLowerCase();
        if(!q)return this.rules;
        return this.rules.filter(r=>(r.name && r.name.toLowerCase().includes(q))||(r.target && r.target.toLowerCase().includes(q))||(r.kind && r.kind.toLowerCase().includes(q))||(r.severity && r.severity.toLowerCase().includes(q)));
      }
      ,async fetchContainers() {
        try {
          const d=await store.apiFetch('/api/v1/containers');
          if(d && Array.isArray(d.containers)) {
            this.availableContainers=d.containers.map(c=> c.name).filter(Boolean);
          }
        } catch(_) {}
      }
      ,async fetchChannelStatus() {
        try {
          const cfg=await store.apiFetch('/api/v1/system/config');
          if(cfg && cfg.notifiers) {
            this.channelStatus.email=!!(cfg.notifiers.email && cfg.notifiers.email.enabled);
            this.channelStatus.discord=!!(cfg.notifiers.discord && cfg.notifiers.discord.enabled);
          }
        } catch(_) {}
      }
      ,setTarget(t) {
        this.ruleForm.target=t;
        if(t==='host') {
          if(this.ruleForm.metricName==='mem_pct')this.ruleForm.metricName='mem_used_pct';
        } else if(t.startsWith('container:') || this.availableContainers.includes(t)) {
          if(this.ruleForm.metricName==='mem_used_pct' || this.ruleForm.metricName==='disk_used_pct') {
            this.ruleForm.metricName='cpu_pct';
          }
        }
      }
      ,getMetricUnit() {
        const m=this.ruleForm.metricName || '';
        if(m.includes('pct'))return '%';
        if(m.includes('bps'))return 'B/s';
        if(m.includes('bytes') || m==='mem_used')return 'Bytes';
        if(m.startsWith('load'))return 'Load';
        return '';
      }
      ,openCreateModal() {
        this.isEditing=false;
        this.previewResult=null;
        this.ruleForm=defaultForm();
        this.fetchContainers();
        this.fetchChannelStatus();
        this.modalOpen=true;
      }
      ,openEditModal(rule) {
        this.isEditing=true;
        this.previewResult=null;
        this.fetchContainers();
        this.fetchChannelStatus();
        const form=defaultForm();
        form.id=rule.id;
        form.name=rule.name;
        form.kind=rule.kind;
        form.target=rule.target;
        form.severity=rule.severity;
        form.for_sec=rule.for_sec;
        form.cooldown_sec=rule.cooldown_sec;
        form.enabled=rule.enabled;
        form.channels= {
          email:false,discord:false
        };
        (rule.channels || []).forEach(ch=> {
          const lc=ch.toLowerCase();
          if(form.channels[lc] !==undefined)form.channels[lc]=true;
        });
        try {
          const c=JSON.parse(rule.condition_json || '{}');
          if(rule.kind==='metric') {
            let m=c.metric || 'cpu_pct';
            if(m==='container_cpu_pct' || m==='host_cpu_pct')m='cpu_pct';
            if(m==='container_mem_pct')m='mem_pct';
            if(m==='host_mem_used_pct')m='mem_used_pct';
            if(m==='host_disk_used_pct')m='disk_used_pct';
            form.metricName=m;
            form.metricOp=c.operator || '>';
            form.metricThreshold=c.threshold !=null ? c.threshold:90.0;
          } else if(rule.kind==='log') {
            form.logPattern=c.pattern || '';
            form.logIsRegex=!!c.is_regex;
            form.logThreshold=c.threshold || 5;
            form.logWindowSec=c.window_sec || 60;
            form.logMinLevel=c.min_level !=null ? c.min_level:5;
          } else if(rule.kind==='container') {
            (c.events || []).forEach(e=> {
              if(form.containerEvents[e] !==undefined)form.containerEvents[e]=true;
            });
            form.containerThreshold=c.threshold || 1;
            form.containerWindowSec=c.window_sec || 0;
          }
        } catch(_) {}
        this.ruleForm=form;
        this.modalOpen=true;
      }
      ,closeModal() {
        this.modalOpen=false;
        this.previewResult=null;
      }
      ,buildConditionJSON() {
        const f=this.ruleForm;
        if(f.kind==='metric') {
          let m=f.metricName || 'cpu_pct';
          if(m==='container_cpu_pct' || m==='host_cpu_pct')m='cpu_pct';
          if(m==='container_mem_pct')m='mem_pct';
          if(m==='host_mem_used_pct')m='mem_used_pct';
          if(m==='host_disk_used_pct')m='disk_used_pct';
          return JSON.stringify({
            metric:m,operator:f.metricOp || '>',threshold:parseFloat(f.metricThreshold)
          });
        }
        if(f.kind==='log') {
          return JSON.stringify( {
            pattern:f.logPattern,is_regex:!!f.logIsRegex,threshold:parseInt(f.logThreshold,10)|| 1,window_sec:parseInt(f.logWindowSec,10)|| 60,min_level:parseInt(f.logMinLevel,10)|| 0
          }
          );
        }
        if(f.kind==='container') {
          const evs=Object.keys(f.containerEvents).filter(k=> f.containerEvents[k]);
          return JSON.stringify( {
            events:evs,threshold:parseInt(f.containerThreshold,10)|| 1,window_sec:parseInt(f.containerWindowSec,10)|| 0
          }
          );
        }
        return '{}';
      }
      ,selectedChannels() {
        return Object.keys(this.ruleForm.channels).filter(k=> this.ruleForm.channels[k]);
      }
      ,async runPreview() {
        if(!this.ruleForm.target) {
          store.toast.show('Target is required for preview','warning');
          return;
        }
        this.previewLoading=true;
        this.previewResult=null;
        try {
          const res=await store.apiFetch('/api/v1/alerts/rules/preview', {
            method:'POST',body: {
              kind:this.ruleForm.kind,target:this.ruleForm.target,severity:this.ruleForm.severity,condition_json:this.buildConditionJSON(),for_sec:parseInt(this.ruleForm.for_sec,10)|| 0,channels:this.selectedChannels()
            }
          }
          );
          this.previewResult=res;
        } catch(_) {
        } finally {
          this.previewLoading=false;
        }
      }
      ,async saveRule() {
        const f=this.ruleForm;
        if(!f.name || !f.name.trim())return store.toast.show('Rule name required','warning');
        if(!f.target || !f.target.trim())return store.toast.show('Target required','warning');
        const chs=this.selectedChannels();
        if(!chs.length)return store.toast.show('Select email or discord channel','warning');
        if(f.kind==='container') {
          const evs=Object.keys(f.containerEvents).filter(k=> f.containerEvents[k]);
          if(!evs.length)return store.toast.show('Select at least one container event','warning');
        }
        this.modalLoading=true;
        try {
          const body= {
            name:f.name.trim(),kind:f.kind,target:f.target.trim(),severity:f.severity,condition_json:this.buildConditionJSON(),for_sec:parseInt(f.for_sec,10)|| 0,cooldown_sec:parseInt(f.cooldown_sec,10)|| 300,channels:chs,enabled:!!f.enabled
          }
          ;
          if(this.isEditing) {
            body.id=f.id;
            await store.apiFetch(`/api/v1/alerts/rules/${f.id}`, {
              method:'PUT',body
            }
            );
            store.toast.show(`Rule "${body.name}" updated`,'success');
          } else {
            await store.apiFetch('/api/v1/alerts/rules', {
              method:'POST',body
            }
            );
            store.toast.show(`Rule "${body.name}" created`,'success');
          }
          this.closeModal();
          await this.fetchRules();
        } catch(_) {
        } finally {
          this.modalLoading=false;
        }
      }
      ,async toggleRule(r) {
        const action=r.enabled ? 'disable':'enable';
        try {
          await store.apiFetch(`/api/v1/alerts/rules/${r.id}/${action}`, {
            method:'POST'
          }
          );
          r.enabled=!r.enabled;
          store.toast.show(`Rule "${r.name}" ${action}d`,'info');
        } catch(_) {
        }
      }
      ,async deleteRule(r) {
        if(!confirm(`Delete rule "${r.name}"?`))return;
        try {
          await store.apiFetch(`/api/v1/alerts/rules/${r.id}`, {
            method:'DELETE'
          }
          );
          store.toast.show(`Rule "${r.name}" deleted`,'info');
          await this.fetchRules();
        } catch(_) {
        }
      }
      ,applyTemplate(tmpl) {
        if(!tmpl)return;
        this.openCreateModal();
        const r=tmpl.rule;
        this.ruleForm.name=r.name;
        this.ruleForm.kind=r.kind;
        this.ruleForm.target=r.target;
        this.ruleForm.severity=r.severity;
        this.ruleForm.for_sec=r.for_sec;
        this.ruleForm.cooldown_sec=r.cooldown_sec;
        try {
          const c=JSON.parse(r.condition_json || '{}');
          if(r.kind==='metric') {
            this.ruleForm.metricName=c.metric || 'host_cpu_pct';
            this.ruleForm.metricOp=c.operator || '>';
            this.ruleForm.metricThreshold=c.threshold !=null ? c.threshold:85;
          } else if(r.kind==='log') {
            this.ruleForm.logPattern=c.pattern || '';
            this.ruleForm.logIsRegex=!!c.is_regex;
            this.ruleForm.logThreshold=c.threshold || 5;
            this.ruleForm.logWindowSec=c.window_sec || 60;
            this.ruleForm.logMinLevel=c.min_level !=null ? c.min_level:5;
          } else if(r.kind==='container') {
            (c.events || []).forEach(e=> {
              if(this.ruleForm.containerEvents[e] !==undefined)this.ruleForm.containerEvents[e]=true;
            }
            );
            this.ruleForm.containerThreshold=c.threshold || 1;
            this.ruleForm.containerWindowSec=c.window_sec || 0;
          }
        } catch(_) {
        }
        store.toast.show(`Loaded template "${tmpl.name}"`,'info');
      }
      ,async exportRules() {
        try {
          const b=await store.apiFetch('/api/v1/alerts/rules/export');
          const blob=new Blob([JSON.stringify(b,null,2)], {
            type:'application/json'
          }
          );
          const url=URL.createObjectURL(blob);
          const a=document.createElement('a');
          a.href=url;
          a.download=`tarsier-alerts-${new Date().toISOString().slice(0,10)}.json`;
          document.body.appendChild(a);
          a.click();
          document.body.removeChild(a);
          URL.revokeObjectURL(url);
          store.toast.show('Exported rules JSON','success');
        } catch(_) {
        }
      }
      ,async importFile(e) {
        const file=e.target.files && e.target.files[0];
        if(!file)return;
        try {
          const text=await file.text();
          const parsed=JSON.parse(text);
          const res=await store.apiFetch('/api/v1/alerts/rules/import', {
            method:'POST',body:parsed
          }
          );
          store.toast.show(`Imported ${res.imported}rules`,'success');
          await this.fetchRules();
        } catch(err) {
          store.toast.show('Import failed:' + err.message,'error');
        } finally {
          e.target.value='';
        }
      }
      ,async sendTest(channel) {
        try {
          await store.apiFetch('/api/v1/alerts/test', {
            method:'POST',body: {
              channel
            }
          }
          );
          store.toast.show(`Test alert dispatched over ${channel}`,'success');
        } catch(_) {
        }
      }
      ,formatCondition(r) {
        if(!r.condition_json)return '--';
        try {
          const c=JSON.parse(r.condition_json);
          if(r.kind==='metric')return `${c.metric}${c.operator}${c.threshold}`;
          if(r.kind==='log')return `"${c.pattern}" ${c.is_regex ? '(regex)':''}≥ ${c.threshold}in ${c.window_sec}s`;
          if(r.kind==='container')return `events:[${(c.events||[]).join(',')}](≥${c.threshold})`;
        } catch(_) {
        }
        return r.condition_json;
      }
      ,formatTime:fmtTime,formatDuration:fmtDur
    }
    ;
  }
  window.AlertsController=AlertsController;
}
)();