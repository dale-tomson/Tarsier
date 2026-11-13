/**
 * Tarsier Main Application & Petite-Vue Reactive Store
 */
(function() {
  'use strict';

  const VALID_ROUTES = ['overview', 'host', 'containers', 'logs', 'alerts', 'settings'];

  // Initialize theme from localStorage or system preference
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

  // Parse initial route from hash
  function getRouteFromHash() {
    const raw = window.location.hash.replace(/^#\/?/, '').trim().toLowerCase();
    return VALID_ROUTES.includes(raw) ? raw : 'overview';
  }

  // Toast manager instance
  const toast = window.ToastState ? window.ToastState() : { show: () => {}, dismiss: () => {} };

  // Petite-Vue Reactive Store
  const store = window.PetiteVue.reactive({
    // Routing state
    route: getRouteFromHash(),
    setRoute(r) {
      if (VALID_ROUTES.includes(r)) {
        this.route = r;
        window.location.hash = '#' + r;
        if (r === 'settings' && this.authenticated) {
          this.loadSettings();
        }
      }
    },

    // Theme state
    theme: savedTheme,
    cycleTheme() {
      const order = ['auto', 'light', 'dark'];
      const nextIdx = (order.indexOf(this.theme) + 1) % order.length;
      this.theme = order[nextIdx];
      localStorage.setItem('tarsier-theme', this.theme);
      applyTheme(this.theme);
      toast.show('Theme set to ' + this.theme, 'info', 2000);
    },

    // Global Time & Refresh Controls
    timeRange: '1h',
    refreshInterval: '10s',

    // Auth state
    authenticated: false,
    user: '',
    loginForm: {
      username: '',
      password: '',
      error: '',
      loading: false
    },

    // Toast
    toast: toast,

    // Settings view data
    systemConfig: null,
    systemHealth: null,

    // API Client wrapper
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

    // Authentication methods
    async checkAuth() {
      try {
        const data = await this.apiFetch('/api/v1/auth/me');
        this.authenticated = true;
        this.user = data.username || 'admin';
        if (this.route === 'settings') {
          this.loadSettings();
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

        if (this.route === 'settings') {
          this.loadSettings();
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
        // Ignore logout network errors
      }
      this.authenticated = false;
      this.user = '';
      this.toast.show('Logged out', 'info');
    },

    // Load Settings & Health
    async loadSettings() {
      try {
        const [cfg, health] = await Promise.all([
          this.apiFetch('/api/v1/system/config'),
          this.apiFetch('/api/v1/system/health')
        ]);
        this.systemConfig = cfg;
        this.systemHealth = health;
      } catch (e) {
        // Handled by apiFetch
      }
    }
  });

  // Hash router sync
  window.addEventListener('hashchange', () => {
    store.route = getRouteFromHash();
    if (store.route === 'settings' && store.authenticated) {
      store.loadSettings();
    }
  });

  // Watch system color scheme changes for 'auto' mode
  if (window.matchMedia) {
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
      if (store.theme === 'auto') applyTheme('auto');
    });
  }

  // Mount PetiteVue
  window.store = store;
  window.PetiteVue.createApp({
    store,
    overviewChart: window.ChartPanel ? window.ChartPanel('overview-chart') : {}
  }).mount();

  // Boot: Check active session
  store.checkAuth();
})();
