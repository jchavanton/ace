// Chart driver for /monitoring. Same shape as bot_report.js but
// without a started_by filter — these panels are global per
// prometheus instance (one node_exporter per host).

(function () {
  const fallbackColors = ["#0d6efd", "#20c997", "#e83e8c", "#ffc107", "#6f42c1", "#fd7e14"];

  const panels = [
    { id: "host_cpu",   panel: "host_cpu",   yLabel: "%",        yMin: 0, yMax: 100 },
    { id: "host_mem",   panel: "host_mem",   yLabel: "%",        yMin: 0, yMax: 100 },
    { id: "host_disk",  panel: "host_disk",  yLabel: "%",        yMin: 0, yMax: 100 },
    { id: "host_load1", panel: "host_load1", yLabel: "load",     yMin: 0 },
    { id: "host_net",   panel: "host_net",   yLabel: "bytes/sec", yMin: 0 },
  ];

  const charts = {};

  function seriesLabel(metric, fallback) {
    return metric.device || metric.mountpoint || metric.instance || fallback;
  }

  function render(panelCfg, resp) {
    const canvas = document.getElementById(panelCfg.id);
    if (!canvas) return;
    const ctx = canvas.getContext("2d");

    const tsSet = new Set();
    resp.result.forEach((s) => s.values.forEach((v) => tsSet.add(v[0])));
    const timestamps = Array.from(tsSet).sort((a, b) => a - b);
    const labels = timestamps.map((t) => new Date(t * 1000));

    const datasets = resp.result.map((s, i) => {
      const map = new Map(s.values.map((v) => [v[0], parseFloat(v[1])]));
      const color = fallbackColors[i % fallbackColors.length];
      return {
        label: seriesLabel(s.metric, panelCfg.panel),
        data: timestamps.map((t) => (map.has(t) ? map.get(t) : null)),
        borderColor: color,
        backgroundColor: color + "55",
        borderWidth: 1,
        fill: false,
        tension: 0.2,
        pointRadius: 0,
        spanGaps: false,
      };
    });

    if (charts[panelCfg.id]) charts[panelCfg.id].destroy();
    charts[panelCfg.id] = new Chart(ctx, {
      type: "line",
      data: { labels, datasets },
      options: {
        responsive: true,
        animation: false,
        interaction: { intersect: false, mode: "index" },
        plugins: {
          legend: { position: "bottom", labels: { boxWidth: 12 } },
          tooltip: { callbacks: { label: (ctx) => `${ctx.dataset.label}: ${ctx.parsed.y}` } },
        },
        scales: {
          x: {
            type: "time",
            time: {
              tooltipFormat: "yyyy-MM-dd HH:mm",
              displayFormats: {
                minute: "HH:mm",
                hour:   "MMM d HH:mm",
                day:    "MMM d",
                week:   "MMM d",
                month:  "MMM yyyy",
              },
            },
            ticks: { maxRotation: 0, autoSkipPadding: 20 },
            grid: { display: false },
          },
          y: {
            beginAtZero: true,
            min: panelCfg.yMin,
            max: panelCfg.yMax,
            title: { display: true, text: panelCfg.yLabel, color: "#6c757d" },
          },
        },
      },
    });
  }

  function loadAll() {
    const rangeMin = document.getElementById("rangeSel").value;
    const loading = document.getElementById("loading");
    loading.classList.remove("d-none");
    const inflight = panels.map((p) =>
      fetch(`/api/metrics/range?panel=${p.panel}&range_min=${rangeMin}`)
        .then((r) => (r.ok ? r.json() : Promise.reject(r.statusText)))
        .then((resp) => render(p, resp))
        .catch((err) => {
          const canvas = document.getElementById(p.id);
          if (canvas) {
            const parent = canvas.parentElement;
            parent.innerHTML = `<div class="text-danger small">Failed to load ${p.panel}: ${err}</div>`;
          }
        })
    );
    Promise.allSettled(inflight).finally(() => loading.classList.add("d-none"));
  }

  const dateAdapter = document.createElement("script");
  dateAdapter.src = "https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js";
  dateAdapter.onload = () => {
    loadAll();
    document.getElementById("rangeSel").addEventListener("change", loadAll);
  };
  document.head.appendChild(dateAdapter);
})();
