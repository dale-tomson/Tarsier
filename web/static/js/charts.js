/**
 * Tarsier Reusable uPlot Chart Management & Shared Cursor Synchronization
 */
(function() {
  'use strict';

  const chartRegistry = new Map();
  const hostSync = (window.uPlot && window.uPlot.sync) ? window.uPlot.sync('host-metrics') : null;

  function getThemeColors() {
    const isDark = document.documentElement.getAttribute('data-theme') === 'dark';
    return {
      text: isDark ? '#cbd5e1' : '#475569',
      grid: isDark ? '#334155' : '#e2e8f0',
      border: isDark ? '#475569' : '#cbd5e1'
    };
  }

  function createChartOpts(opts) {
    const theme = getThemeColors();
    const series = [{}];

    (opts.series || []).forEach(s => {
      series.push({
        label: s.label,
        stroke: s.stroke || '#2563eb',
        width: s.width || 1.5,
        fill: s.fill || null,
        value: (u, v) => (v != null && opts.valFmt ? opts.valFmt(v) : (v != null ? v.toFixed(1) : '--'))
      });
    });

    const axes = [
      {
        stroke: theme.text,
        grid: { stroke: theme.grid, width: 1 }
      },
      {
        stroke: theme.text,
        grid: { stroke: theme.grid, width: 1 },
        values: (u, vals) => vals.map(v => (v != null && opts.valFmt ? opts.valFmt(v) : (v != null ? v.toFixed(0) : ''))),
        range: opts.range || null
      }
    ];

    return {
      title: opts.title || '',
      width: opts.width || 400,
      height: opts.height || 200,
      cursor: {
        sync: hostSync ? { key: 'host-metrics', setSeries: true } : null,
        drag: { setScale: false }
      },
      legend: {
        show: true
      },
      series: series,
      axes: axes
    };
  }

  function mountChart(el, id, options, initialData) {
    if (!window.uPlot || !el) return null;
    unmountChart(id);

    const rect = el.getBoundingClientRect();
    const width = Math.floor(rect.width) || 400;
    options.width = width;

    const uplotOpts = createChartOpts(options);
    const data = initialData || [[], []];
    const chart = new window.uPlot(uplotOpts, data, el);

    let observer = null;
    if (window.ResizeObserver) {
      observer = new ResizeObserver(entries => {
        for (const entry of entries) {
          const w = Math.floor(entry.contentRect.width);
          if (w > 0 && chart) {
            chart.setSize({ width: w, height: options.height || 200 });
          }
        }
      });
      observer.observe(el);
    }

    chartRegistry.set(id, { chart, observer });
    return chart;
  }

  function updateChart(id, data) {
    const entry = chartRegistry.get(id);
    if (entry && entry.chart && data && data.length > 0) {
      entry.chart.setData(data);
    }
  }

  function unmountChart(id) {
    const entry = chartRegistry.get(id);
    if (entry) {
      if (entry.observer) entry.observer.disconnect();
      if (entry.chart) entry.chart.destroy();
      chartRegistry.delete(id);
    }
  }

  window.TarsierCharts = {
    mountChart,
    updateChart,
    unmountChart,
    getThemeColors
  };
})();
