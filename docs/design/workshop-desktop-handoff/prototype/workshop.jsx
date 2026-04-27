// Spire Desktop — Workshop view
// Browse formulas (the bead lifecycle DAGs); inspect each step; trace paths.
//
// Layout strategy: column-packed DAG via Kahn-style layering of `needs[]`.
// Renders steps as cards inline (not abstract circles) so step kind, action,
// title, when-clause, and produces are all readable at a glance — that is
// the whole point of looking at a formula.

// ── DAG layering: assign each step a column (0,1,2…) by longest-path topo ──
function layerSteps(steps) {
  const byName = Object.fromEntries(steps.map(s => [s.name, s]));
  const layer  = {};
  function dfs(name, seen = new Set()) {
    if (layer[name] != null) return layer[name];
    if (seen.has(name)) return 0; // cycle guard (resets create them)
    seen.add(name);
    const s = byName[name];
    const needs = s?.needs || [];
    if (needs.length === 0) { layer[name] = 0; return 0; }
    const max = Math.max(...needs.map(n => byName[n] ? dfs(n, seen) : -1));
    layer[name] = max + 1;
    return layer[name];
  }
  steps.forEach(s => dfs(s.name));
  // Group steps per layer, preserving authored order within layer
  const byLayer = {};
  steps.forEach(s => {
    const L = layer[s.name];
    (byLayer[L] = byLayer[L] || []).push(s);
  });
  const cols = Object.keys(byLayer).map(Number).sort((a,b) => a-b).map(L => byLayer[L]);
  return { cols, layer };
}

// Compute pixel positions for each step card in the canvas
function layoutFormula(formula, opts = {}) {
  const COL_W   = opts.colW   ?? 200;
  const ROW_H   = opts.rowH   ?? 78;
  const PAD_X   = opts.padX   ?? 24;
  const PAD_Y   = opts.padY   ?? 24;
  const CARD_W  = opts.cardW  ?? 168;
  const CARD_H  = opts.cardH  ?? 60;

  const { cols, layer } = layerSteps(formula.steps);
  const positions = {};
  cols.forEach((stepsInCol, ci) => {
    stepsInCol.forEach((s, ri) => {
      positions[s.name] = {
        x: PAD_X + ci * COL_W,
        y: PAD_Y + ri * ROW_H,
        w: CARD_W,
        h: CARD_H,
        col: ci,
        row: ri,
      };
    });
  });
  const width  = PAD_X + cols.length * COL_W + 20;
  const height = PAD_Y + Math.max(...cols.map(c => c.length)) * ROW_H + PAD_Y;
  return { positions, width, height, cols, layer };
}

// ── Main view ──────────────────────────────────────────────────────
function WorkshopView({ onOpenBead, initialFormula }) {
  const [selectedFormula, setFormula] = React.useState(initialFormula || "task-default");
  const [selectedStep,    setStep]    = React.useState(null);
  const [highlightedPath, setPath]    = React.useState(null);
  const [tab,             setTab]     = React.useState("graph"); // graph|steps|vars|source|validation

  // External nav (e.g. from BeadDetail) requests a different formula
  React.useEffect(() => {
    if (initialFormula && initialFormula !== selectedFormula) setFormula(initialFormula);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialFormula]);

  const formula = getFormula(selectedFormula);
  const layout  = React.useMemo(() => layoutFormula(formula), [formula]);

  // Reset step selection when switching formulas
  React.useEffect(() => { setStep(null); setPath(null); }, [selectedFormula]);

  return (
    <div style={{ display: 'flex', height: '100%', background: 'var(--bg-1)' }}>
      <FormulaRail
        list={FORMULAS}
        selected={selectedFormula}
        onSelect={setFormula}
      />

      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <FormulaHeader formula={formula} tab={tab} onTab={setTab}/>

        <div style={{ flex: 1, display: 'flex', overflow: 'hidden', minWidth: 0 }}>
          <div style={{ flex: 1, position: 'relative', overflow: 'hidden', minWidth: 0 }}>
            {tab === "graph" && (
              <GraphCanvas
                formula={formula}
                layout={layout}
                selectedStep={selectedStep}
                highlightedPath={highlightedPath}
                onSelectStep={setStep}
              />
            )}
            {tab === "steps"      && <StepsTable      formula={formula} onSelect={setStep}/>}
            {tab === "vars"       && <VarsTable       formula={formula}/>}
            {tab === "source"     && <SourceView      formula={formula}/>}
            {tab === "validation" && <ValidationView  formula={formula} onSelectStep={setStep}/>}
          </div>

          <StepInspector
            formula={formula}
            step={selectedStep ? formula.steps.find(s => s.name === selectedStep) : null}
            highlightedPath={highlightedPath}
            onPath={setPath}
            onClose={() => setStep(null)}
            onOpenFormula={setFormula}
          />
        </div>
      </div>
    </div>
  );
}

// ── Left rail: formula list ────────────────────────────────────────
function FormulaRail({ list, selected, onSelect }) {
  const embedded = list.filter(f => f.source === "embedded");
  const custom   = list.filter(f => f.source === "custom");
  return (
    <aside style={{
      width: 252, flexShrink: 0,
      borderRight: '1px solid var(--line-2)',
      background: 'var(--bg-1)',
      display: 'flex', flexDirection: 'column',
      overflow: 'hidden',
    }}>
      <div style={{
        height: 38, display: 'flex', alignItems: 'center', gap: 8,
        padding: '0 14px', borderBottom: '1px solid var(--line-2)',
      }}>
        <span className="mono" style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.6, color: 'var(--ink-0)' }}>
          FORMULAS
        </span>
        <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)' }}>{list.length}</span>
        <div style={{ flex: 1 }}/>
        <button title="New custom formula" className="mono" style={{
          width: 22, height: 22, display: 'flex', alignItems: 'center', justifyContent: 'center',
          color: 'var(--ink-2)', border: '1px solid var(--line-2)', borderRadius: 3,
        }}><Icon.plus/></button>
      </div>

      <div style={{ flex: 1, overflow: 'auto' }}>
        <RailSection label="EMBEDDED" subtitle="shipped with archmage" items={embedded} selected={selected} onSelect={onSelect}/>
        <RailSection label="CUSTOM"   subtitle={`${custom.length} authored in tower`} items={custom} selected={selected} onSelect={onSelect}/>
      </div>

      <div style={{
        padding: '8px 14px', borderTop: '1px solid var(--line-2)',
        display: 'flex', alignItems: 'center', gap: 8,
        fontSize: 10.5, color: 'var(--ink-3)',
      }} className="mono">
        <span>v3 schema</span>
        <span style={{ color: 'var(--line-3)' }}>│</span>
        <span>2 issues</span>
      </div>
    </aside>
  );
}

function RailSection({ label, subtitle, items, selected, onSelect }) {
  return (
    <div style={{ padding: '10px 0 4px' }}>
      <div style={{ padding: '4px 14px 6px', display: 'flex', alignItems: 'baseline', gap: 6 }}>
        <span className="mono" style={{ fontSize: 9.5, fontWeight: 700, letterSpacing: 0.7, color: 'var(--ink-3)' }}>{label}</span>
        <span className="mono" style={{ fontSize: 10, color: 'var(--ink-4)' }}>{subtitle}</span>
      </div>
      {items.map(f => {
        const active = f.name === selected;
        const hasIssues = (f.issues || []).length > 0;
        const hasError  = (f.issues || []).some(i => i.level === "error");
        return (
          <button key={f.name} onClick={() => onSelect(f.name)} style={{
            display: 'block', textAlign: 'left', width: '100%',
            padding: '8px 12px 9px 14px',
            borderLeft: `2px solid ${active ? 'var(--sig-green)' : 'transparent'}`,
            background: active ? 'var(--bg-3)' : 'transparent',
            overflow: 'hidden',
          }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3, minWidth: 0 }}>
              <span className="mono" style={{ fontSize: 12, fontWeight: 600, color: active ? 'var(--ink-0)' : 'var(--ink-1)',
                overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0, flex: 1,
              }}>
                {f.name}
              </span>
              {hasIssues && (
                <span title={`${f.issues.length} issue${f.issues.length === 1 ? '' : 's'}`} style={{
                  width: 6, height: 6, borderRadius: '50%',
                  background: hasError ? 'var(--sig-red)' : 'var(--sig-amber)',
                }}/>
              )}
            </div>
            <div style={{
              fontSize: 11.5, color: 'var(--ink-2)', lineHeight: 1.4,
              display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden',
              marginBottom: 5,
            }}>{f.description}</div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
              {(f.default_for || []).map(t => (
                <span key={t} className="mono" style={{
                  fontSize: 9.5, padding: '1px 5px',
                  color: 'var(--sig-green)',
                  background: 'rgba(79,227,138,0.10)',
                  border: '1px solid rgba(79,227,138,0.25)',
                  borderRadius: 3, letterSpacing: 0.4,
                }}>default · {t}</span>
              ))}
              <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)' }}>
                {f.steps.length} steps
              </span>
            </div>
          </button>
        );
      })}
    </div>
  );
}

// ── Header: formula identity + tabs ─────────────────────────────────
function FormulaHeader({ formula, tab, onTab }) {
  const issues = formula.issues || [];
  return (
    <div style={{
      borderBottom: '1px solid var(--line-2)',
      background: 'var(--bg-1)',
      padding: '14px 22px 0',
    }}>
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: 14 }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 9, marginBottom: 6, flexWrap: 'wrap' }}>
            <h1 style={{ margin: 0, fontSize: 17, fontWeight: 600, color: 'var(--ink-0)', letterSpacing: 0, whiteSpace: 'nowrap' }} className="mono">
              {formula.name}
            </h1>
            <span className="mono" style={{
              fontSize: 10, padding: '1px 6px', letterSpacing: 0.4,
              color: 'var(--ink-2)', background: 'var(--bg-3)',
              border: '1px solid var(--line-2)', borderRadius: 3,
            }}>v{formula.version}</span>
            <span className="mono" style={{
              fontSize: 10, padding: '1px 6px', letterSpacing: 0.4,
              color: formula.source === "embedded" ? 'var(--sig-blue)' : 'var(--sig-purple)',
              background: formula.source === "embedded" ? 'rgba(90,165,255,0.10)' : 'rgba(179,140,255,0.10)',
              border: `1px solid ${formula.source === "embedded" ? 'rgba(90,165,255,0.25)' : 'rgba(179,140,255,0.25)'}`,
              borderRadius: 3, textTransform: 'uppercase',
            }}>{formula.source}</span>
            {(formula.default_for || []).map(t => (
              <span key={t} className="mono" style={{
                fontSize: 10, padding: '1px 6px',
                color: 'var(--sig-green)',
                background: 'rgba(79,227,138,0.10)',
                border: '1px solid rgba(79,227,138,0.25)',
                borderRadius: 3, letterSpacing: 0.4,
              }}>default · {t}</span>
            ))}
            {issues.length > 0 && (
              <span className="mono" style={{
                fontSize: 10, padding: '1px 6px',
                color: issues.some(i => i.level === "error") ? 'var(--sig-red)' : 'var(--sig-amber)',
                background: issues.some(i => i.level === "error") ? 'rgba(255,93,93,0.10)' : 'rgba(247,201,72,0.10)',
                border: `1px solid ${issues.some(i => i.level === "error") ? 'rgba(255,93,93,0.30)' : 'rgba(247,201,72,0.30)'}`,
                borderRadius: 3, letterSpacing: 0.4,
                display: 'inline-flex', alignItems: 'center', gap: 4,
              }}>
                ⚠ {issues.length} issue{issues.length === 1 ? '' : 's'}
              </span>
            )}
          </div>
          <div style={{ fontSize: 12.5, color: 'var(--ink-2)', lineHeight: 1.5, maxWidth: 720 }}>
            {formula.description}
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 14, marginTop: 8 }} className="mono">
            <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
              ENTRY: <span style={{ color: 'var(--sig-green)' }}>{formula.entry}</span>
            </span>
            <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
              {formula.steps.length} steps
            </span>
            <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
              {formula.steps.filter(s => s.terminal).length} terminal
            </span>
            <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
              {(formula.paths || []).length} paths
            </span>
            <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
              {(formula.workspaces || []).length} workspace{(formula.workspaces || []).length === 1 ? '' : 's'}
            </span>
            {formula.stats && (
              <>
                <span style={{ color: 'var(--line-3)' }}>│</span>
                <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
                  runs 30d: <span style={{ color: 'var(--ink-1)' }}>{formula.stats.runs}</span>
                </span>
                <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
                  success: <span style={{ color: formula.stats.success >= 0.85 ? 'var(--sig-green)' : 'var(--sig-amber)' }}>{Math.round(formula.stats.success * 100)}%</span>
                </span>
                <span style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>
                  p50: <span style={{ color: 'var(--ink-1)' }}>{formula.stats.p50_duration}</span>
                </span>
              </>
            )}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 6, paddingTop: 4 }}>
          <button className="mono" style={{
            display: 'inline-flex', alignItems: 'center', gap: 6,
            height: 28, padding: '0 12px', fontSize: 11, fontWeight: 600, letterSpacing: 0.3,
            color: 'var(--ink-1)', background: 'var(--bg-3)',
            border: '1px solid var(--line-3)', borderRadius: 4,
          }}>FORK COPY</button>
          <button className="mono" style={{
            display: 'inline-flex', alignItems: 'center', gap: 6,
            height: 28, padding: '0 12px', fontSize: 11, fontWeight: 600, letterSpacing: 0.3,
            color: '#0b0e14', background: 'var(--sig-green)', borderRadius: 4,
          }}><Icon.bolt/> RUN ON BEAD…</button>
        </div>
      </div>

      {/* Tabs */}
      <div style={{ display: 'flex', gap: 0, marginTop: 14 }}>
        {[
          { id: "graph",      label: "GRAPH" },
          { id: "steps",      label: "STEPS",      count: formula.steps.length },
          { id: "vars",       label: "VARS",       count: (formula.vars || []).length },
          { id: "source",     label: "SOURCE" },
          { id: "validation", label: "VALIDATION", count: issues.length, warn: issues.length > 0 },
        ].map(t => {
          const active = tab === t.id;
          return (
            <button key={t.id} onClick={() => onTab(t.id)} className="mono" style={{
              padding: '8px 14px',
              fontSize: 10.5, fontWeight: 600, letterSpacing: 0.5,
              color: active ? 'var(--ink-0)' : 'var(--ink-2)',
              borderBottom: active ? '2px solid var(--sig-green)' : '2px solid transparent',
              marginBottom: -1,
              display: 'inline-flex', alignItems: 'center', gap: 5,
            }}>
              {t.label}
              {t.count != null && (
                <span style={{
                  fontSize: 9.5, padding: '0 5px', minWidth: 14, height: 14,
                  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
                  background: t.warn ? 'rgba(247,201,72,0.15)' : 'var(--bg-3)',
                  color: t.warn ? 'var(--sig-amber)' : 'var(--ink-3)',
                  border: `1px solid ${t.warn ? 'rgba(247,201,72,0.30)' : 'var(--line-2)'}`,
                  borderRadius: 7,
                }}>{t.count}</span>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}

Object.assign(window, { WorkshopView, layoutFormula, layerSteps });
