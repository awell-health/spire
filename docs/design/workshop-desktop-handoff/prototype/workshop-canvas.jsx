// Spire Desktop — Workshop graph canvas, inspector, and supporting tabs

// ── Graph canvas: column-packed DAG with step cards + edge routing ──
function GraphCanvas({ formula, layout, selectedStep, highlightedPath, onSelectStep }) {
  const { positions, width, height } = layout;

  // pathSet: edges that are part of the highlighted path
  const pathEdges = React.useMemo(() => {
    if (!highlightedPath) return null;
    const s = new Set();
    for (let i = 0; i < highlightedPath.length - 1; i++) {
      s.add(`${highlightedPath[i]}::${highlightedPath[i+1]}`);
    }
    return s;
  }, [highlightedPath]);
  const pathStepSet = React.useMemo(() =>
    highlightedPath ? new Set(highlightedPath) : null
  , [highlightedPath]);

  return (
    <div style={{ width: '100%', height: '100%', overflow: 'auto', background: 'var(--bg-1)' }} className="grid-bg">
      <svg width={width} height={height} style={{ display: 'block' }}>
        <defs>
          <marker id="wf-arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--ink-3)"/>
          </marker>
          <marker id="wf-arr-active" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--sig-green)"/>
          </marker>
          <marker id="wf-arr-reset" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--sig-amber)"/>
          </marker>
        </defs>

        {/* Edges */}
        {(formula.edges || []).map((e, i) => {
          const a = positions[e.from], b = positions[e.to];
          if (!a || !b) return null;
          const isReset = e.kind === "reset";
          const onPath = pathEdges?.has(`${e.from}::${e.to}`);
          const dim = pathEdges && !onPath;
          const x1 = a.x + a.w, y1 = a.y + a.h / 2;
          const x2 = b.x,       y2 = b.y + b.h / 2;
          // For reset edges going backward (b.col <= a.col), route a curve up over the cards
          if (isReset || b.col <= a.col) {
            const dy = -28 - Math.abs(a.row - b.row) * 4;
            const path = `M ${a.x + a.w / 2} ${a.y}
                          C ${a.x + a.w / 2} ${a.y + dy},
                            ${b.x + b.w / 2} ${b.y + dy},
                            ${b.x + b.w / 2} ${b.y}`;
            return (
              <g key={i} opacity={dim ? 0.18 : 1}>
                <path d={path} fill="none"
                  stroke={onPath ? 'var(--sig-green)' : 'var(--sig-amber)'}
                  strokeWidth={onPath ? 1.6 : 1.2}
                  strokeDasharray="3,3"
                  markerEnd={onPath ? "url(#wf-arr-active)" : "url(#wf-arr-reset)"}/>
                <text x={(a.x + b.x + a.w + b.w) / 4} y={a.y + dy + 2}
                  textAnchor="middle"
                  fontFamily="JetBrains Mono" fontSize="9" fill="var(--sig-amber)">
                  reset
                </text>
              </g>
            );
          }
          // Forward edge: orthogonal routing with rounded corner
          const midX = (x1 + x2) / 2;
          const path = `M ${x1} ${y1} C ${midX} ${y1}, ${midX} ${y2}, ${x2} ${y2}`;
          return (
            <g key={i} opacity={dim ? 0.18 : 1}>
              <path d={path} fill="none"
                stroke={onPath ? 'var(--sig-green)' : 'var(--ink-3)'}
                strokeWidth={onPath ? 1.6 : 1.0}
                opacity={onPath ? 1 : 0.55}
                markerEnd={onPath ? "url(#wf-arr-active)" : "url(#wf-arr)"}/>
              {e.when && (
                <g>
                  <rect x={midX - 32} y={(y1 + y2) / 2 - 7} width="64" height="14" rx="2"
                    fill="var(--bg-2)" stroke="var(--line-2)" strokeWidth="0.5"/>
                  <text x={midX} y={(y1 + y2) / 2 + 3}
                    textAnchor="middle"
                    fontFamily="JetBrains Mono" fontSize="9"
                    fill={onPath ? 'var(--sig-green)' : 'var(--ink-2)'}>
                    {truncateExpr(e.when, 12)}
                  </text>
                </g>
              )}
            </g>
          );
        })}

        {/* Step cards */}
        {formula.steps.map(s => {
          const p = positions[s.name];
          if (!p) return null;
          const sel  = selectedStep === s.name;
          const onPath = pathStepSet?.has(s.name);
          const dim    = pathStepSet && !onPath;
          return (
            <StepCard key={s.name} step={s} pos={p}
              selected={sel} dim={dim} entry={s.name === formula.entry}
              onClick={() => onSelectStep(s.name)}/>
          );
        })}
      </svg>

      <CanvasLegend/>
    </div>
  );
}

function truncateExpr(s, n) {
  return s.length > n ? s.slice(0, n) + "…" : s;
}

function StepCard({ step, pos, selected, dim, entry, onClick }) {
  const meta = KIND_META[step.kind] || KIND_META.op;
  const terminal = step.terminal;
  const ringColor = selected ? 'var(--sig-green)'
                  : terminal ? 'var(--ink-3)'
                  : meta.ring;
  return (
    <g transform={`translate(${pos.x}, ${pos.y})`}
       opacity={dim ? 0.3 : 1}
       style={{ cursor: 'pointer' }}
       onClick={onClick}>
      {/* entry indicator */}
      {entry && (
        <g>
          <circle cx="-9" cy={pos.h / 2} r="4" fill="var(--sig-green)"/>
          <circle cx="-9" cy={pos.h / 2} r="6" fill="none" stroke="var(--sig-green)" strokeWidth="0.8" opacity="0.5"/>
        </g>
      )}
      {/* terminal marker — double-line on right edge */}
      {terminal && (
        <line x1={pos.w + 3} y1="2" x2={pos.w + 3} y2={pos.h - 2}
          stroke="var(--ink-3)" strokeWidth="2"/>
      )}
      <rect width={pos.w} height={pos.h} rx="4"
        fill={selected ? 'var(--bg-4)' : 'var(--bg-3)'}
        stroke={ringColor} strokeWidth={selected ? 1.5 : 1}/>
      {/* kind ribbon (left strip) */}
      <rect width="3" height={pos.h} rx="0" fill={meta.fg} opacity="0.85"/>
      {/* kind label */}
      <text x="10" y="14" fontFamily="JetBrains Mono" fontSize="9.5" fontWeight="700"
        fill={meta.fg} letterSpacing="0.6">
        {meta.label}
      </text>
      {/* role pill if present */}
      {step.role && (
        <g>
          <rect x={pos.w - 50} y="6" width="44" height="13" rx="2"
            fill={ROLE_COLOR[step.role]?.bg || 'var(--bg-4)'}
            stroke={ROLE_COLOR[step.role]?.fg || 'var(--line-3)'} strokeWidth="0.5"/>
          <text x={pos.w - 28} y="15" textAnchor="middle"
            fontFamily="JetBrains Mono" fontSize="8.5" fontWeight="600" letterSpacing="0.5"
            fill={ROLE_COLOR[step.role]?.fg || 'var(--ink-2)'}>
            {step.role.toUpperCase()}
          </text>
        </g>
      )}
      {/* step name */}
      <text x="10" y="32" fontFamily="JetBrains Mono" fontSize="11.5" fontWeight="600" fill="var(--ink-0)">
        {step.name}
      </text>
      {/* action / title */}
      <text x="10" y="48" fontFamily="Inter Tight, sans-serif" fontSize="10.5" fill="var(--ink-2)">
        {clipText(step.title || step.action || "", 22)}
      </text>
    </g>
  );
}

function clipText(s, n) { return s.length > n ? s.slice(0, n - 1) + "…" : s; }

function CanvasLegend() {
  return (
    <div style={{
      position: 'absolute', bottom: 14, left: 14,
      display: 'flex', flexDirection: 'column', gap: 6,
      padding: '9px 11px',
      background: 'rgba(15,19,27,0.85)',
      backdropFilter: 'blur(4px)',
      border: '1px solid var(--line-2)', borderRadius: 5,
    }} className="mono">
      <div style={{ fontSize: 9.5, color: 'var(--ink-3)', letterSpacing: 0.6, marginBottom: 2 }}>LEGEND</div>
      <Legend label="OP"       color={KIND_META.op.fg}       desc="single action"/>
      <Legend label="CALL"     color={KIND_META.call.fg}     desc="invoke sub-graph"/>
      <Legend label="DISPATCH" color={KIND_META.dispatch.fg} desc="spawn child beads"/>
      <div style={{ height: 4 }}/>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <span style={{ width: 10, height: 10, borderRadius: '50%', background: 'var(--sig-green)' }}/>
        <span style={{ fontSize: 10, color: 'var(--ink-2)' }}>entry</span>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <span style={{ width: 2, height: 10, background: 'var(--ink-3)' }}/>
        <span style={{ fontSize: 10, color: 'var(--ink-2)' }}>terminal</span>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <svg width="20" height="8"><line x1="0" y1="4" x2="20" y2="4" stroke="var(--sig-amber)" strokeWidth="1.2" strokeDasharray="3,3"/></svg>
        <span style={{ fontSize: 10, color: 'var(--ink-2)' }}>reset edge</span>
      </div>
    </div>
  );
}

function Legend({ label, color, desc }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
      <span style={{ fontSize: 9.5, fontWeight: 700, letterSpacing: 0.5, color, width: 56 }}>{label}</span>
      <span style={{ fontSize: 10, color: 'var(--ink-2)' }}>{desc}</span>
    </div>
  );
}

// ── Right: step inspector + path explorer ─────────────────────────
function StepInspector({ formula, step, highlightedPath, onPath, onClose, onOpenFormula }) {
  return (
    <aside style={{
      width: 340, flexShrink: 0,
      borderLeft: '1px solid var(--line-2)',
      background: 'var(--bg-2)',
      display: 'flex', flexDirection: 'column',
      overflow: 'hidden',
    }}>
      <div style={{
        height: 38, display: 'flex', alignItems: 'center', gap: 8,
        padding: '0 14px', borderBottom: '1px solid var(--line-2)',
        background: 'var(--bg-1)',
      }}>
        <span className="mono" style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.6, color: 'var(--ink-0)' }}>
          {step ? "STEP" : "PATHS"}
        </span>
        <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)' }}>
          {step ? step.name : `${(formula.paths || []).length} traces`}
        </span>
        <div style={{ flex: 1 }}/>
        {step && (
          <button onClick={onClose} title="Clear selection" style={{
            width: 22, height: 22, display: 'flex', alignItems: 'center', justifyContent: 'center',
            color: 'var(--ink-3)',
          }}><Icon.close/></button>
        )}
      </div>

      <div style={{ flex: 1, overflow: 'auto' }}>
        {step
          ? <StepDetails step={step} formula={formula} onOpenFormula={onOpenFormula}/>
          : <PathExplorer formula={formula} highlightedPath={highlightedPath} onPath={onPath}/>}
      </div>
    </aside>
  );
}

function StepDetails({ step, formula, onOpenFormula }) {
  const meta = KIND_META[step.kind] || KIND_META.op;
  const incoming = (formula.edges || []).filter(e => e.to === step.name);
  const outgoing = (formula.edges || []).filter(e => e.from === step.name);
  return (
    <div style={{ padding: '14px 16px 24px' }}>
      {/* Kind banner */}
      <div style={{
        padding: '6px 9px', borderRadius: 4,
        background: meta.bg, border: `1px solid ${meta.ring}`,
        display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12,
      }}>
        <span className="mono" style={{ fontSize: 10.5, fontWeight: 700, color: meta.fg, letterSpacing: 0.6 }}>
          {meta.label}
        </span>
        <span style={{ fontSize: 11, color: 'var(--ink-2)' }}>{meta.description}</span>
      </div>

      <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink-0)', marginBottom: 4 }}>
        {step.title || step.name}
      </div>
      <div className="mono" style={{ fontSize: 11, color: 'var(--ink-3)', marginBottom: 14 }}>
        {step.name} {step.terminal && <span style={{ color: 'var(--sig-amber)', marginLeft: 6 }}>· TERMINAL</span>}
      </div>

      <DetailRow label="ACTION" value={<code className="mono" style={{ fontSize: 11.5, color: 'var(--sig-cyan)' }}>{step.action}</code>}/>
      {step.flow  && <DetailRow label="FLOW"  value={<code className="mono" style={{ fontSize: 11.5, color: 'var(--ink-1)' }}>{step.flow}</code>}/>}
      {step.role  && <DetailRow label="ROLE"  value={<RoleBadge role={step.role}/>}/>}
      {step.model && <DetailRow label="MODEL" value={<code className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{step.model}</code>}/>}
      {step.timeout && <DetailRow label="TIMEOUT" value={<code className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{step.timeout}</code>}/>}
      {step.workspace && <DetailRow label="WORKSPACE" value={<code className="mono" style={{ fontSize: 11, color: 'var(--sig-amber)' }}>{step.workspace}</code>}/>}

      {/* Sub-graph drilldown */}
      {step.kind === "call" && step.graph && (
        <div style={{ marginTop: 14, padding: '10px 11px',
          background: 'rgba(179,140,255,0.08)',
          border: '1px solid rgba(179,140,255,0.30)',
          borderRadius: 5,
        }}>
          <div className="mono" style={{ fontSize: 9.5, letterSpacing: 0.6, color: 'var(--sig-purple)', marginBottom: 5 }}>
            CALLS SUB-GRAPH
          </div>
          <button onClick={() => onOpenFormula(step.graph)} style={{
            display: 'flex', alignItems: 'center', gap: 6, width: '100%',
            padding: '6px 8px', borderRadius: 3,
            background: 'rgba(179,140,255,0.12)',
            border: '1px solid rgba(179,140,255,0.30)',
            color: 'var(--sig-purple)',
          }}>
            <span className="mono" style={{ fontSize: 12, fontWeight: 600 }}>{step.graph}</span>
            <div style={{ flex: 1 }}/>
            <Icon.chevR/>
          </button>
        </div>
      )}

      {/* When clause */}
      {step.when && (
        <div style={{ marginTop: 12 }}>
          <SectionLabel>WHEN</SectionLabel>
          <pre className="mono" style={{
            margin: 0, padding: '8px 10px',
            fontSize: 11, color: 'var(--sig-amber)', lineHeight: 1.5,
            background: 'var(--bg-3)', border: '1px solid var(--line-2)', borderRadius: 4,
            whiteSpace: 'pre-wrap', wordBreak: 'break-word',
          }}>{step.when}</pre>
        </div>
      )}

      {/* with: */}
      {step.with && Object.keys(step.with).length > 0 && (
        <div style={{ marginTop: 12 }}>
          <SectionLabel>WITH</SectionLabel>
          <div style={{ background: 'var(--bg-3)', border: '1px solid var(--line-2)', borderRadius: 4, padding: '6px 10px' }}>
            {Object.entries(step.with).map(([k, v]) => (
              <div key={k} className="mono" style={{ fontSize: 11, lineHeight: 1.6, display: 'flex', gap: 8 }}>
                <span style={{ color: 'var(--ink-3)' }}>{k}:</span>
                <span style={{ color: 'var(--ink-1)', wordBreak: 'break-word' }}>
                  {typeof v === "string" && v.length > 60 ? v.slice(0, 57) + "…" : String(v)}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Resets */}
      {step.resets && step.resets.length > 0 && (
        <div style={{ marginTop: 12 }}>
          <SectionLabel warn>RESETS</SectionLabel>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>
            {step.resets.map(r => (
              <span key={r} className="mono" style={{
                fontSize: 10.5, padding: '2px 7px',
                color: 'var(--sig-amber)',
                background: 'rgba(247,201,72,0.10)',
                border: '1px solid rgba(247,201,72,0.30)',
                borderRadius: 3,
              }}>{r}</span>
            ))}
          </div>
        </div>
      )}

      {/* Edges */}
      <div style={{ marginTop: 14 }}>
        <SectionLabel>NEEDS · {incoming.length}</SectionLabel>
        {incoming.length === 0
          ? <div className="mono" style={{ fontSize: 11, color: 'var(--ink-3)', fontStyle: 'italic' }}>none — entry candidate</div>
          : incoming.map((e, i) => <EdgeChip key={i} edge={e} dir="from"/>)}
      </div>
      <div style={{ marginTop: 12 }}>
        <SectionLabel>LEADS TO · {outgoing.length}</SectionLabel>
        {outgoing.length === 0
          ? <div className="mono" style={{ fontSize: 11, color: 'var(--ink-3)', fontStyle: 'italic' }}>none — terminal step</div>
          : outgoing.map((e, i) => <EdgeChip key={i} edge={e} dir="to"/>)}
      </div>
    </div>
  );
}

function DetailRow({ label, value }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, marginBottom: 8 }}>
      <span className="mono" style={{ fontSize: 9.5, letterSpacing: 0.6, color: 'var(--ink-3)', width: 76, flexShrink: 0 }}>{label}</span>
      <span style={{ flex: 1 }}>{value}</span>
    </div>
  );
}

function SectionLabel({ children, warn }) {
  return (
    <div className="mono" style={{
      fontSize: 9.5, letterSpacing: 0.6,
      color: warn ? 'var(--sig-amber)' : 'var(--ink-3)',
      marginBottom: 6,
    }}>{children}</div>
  );
}

function EdgeChip({ edge, dir }) {
  const target = dir === "from" ? edge.from : edge.to;
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '3px 0' }} className="mono">
      <span style={{ fontSize: 10, color: 'var(--ink-3)' }}>{dir === "from" ? "←" : "→"}</span>
      <span style={{ fontSize: 11.5, color: 'var(--ink-1)' }}>{target}</span>
      {edge.when && (
        <span style={{
          fontSize: 10, padding: '0 5px',
          color: 'var(--sig-amber)',
          background: 'rgba(247,201,72,0.10)',
          border: '1px solid rgba(247,201,72,0.25)',
          borderRadius: 3,
        }}>when {truncateExpr(edge.when, 18)}</span>
      )}
      {edge.kind === "reset" && (
        <span style={{ fontSize: 10, color: 'var(--sig-amber)' }}>· reset</span>
      )}
    </div>
  );
}

function PathExplorer({ formula, highlightedPath, onPath }) {
  const paths = formula.paths || [];
  return (
    <div style={{ padding: '10px 14px 24px' }}>
      <div style={{ fontSize: 12, color: 'var(--ink-2)', lineHeight: 1.5, marginBottom: 12 }}>
        Click a trace to highlight that path through the graph. Each trace is a possible run from <code className="mono" style={{ color: 'var(--sig-green)' }}>{formula.entry}</code> to a terminal step.
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        {paths.map((p, i) => {
          const active = highlightedPath && highlightedPath.length === p.length &&
                         highlightedPath.every((s, j) => s === p[j]);
          const last = p[p.length - 1];
          const lastStep = formula.steps.find(s => s.name === last);
          const outcome = lastStep?.with?.status || last;
          const tone = outcome === "closed" ? 'var(--sig-green)'
                     : outcome === "discard" ? 'var(--ink-3)'
                     : outcome === "escalate" ? 'var(--sig-red)'
                     : 'var(--sig-amber)';
          return (
            <button key={i} onClick={() => onPath(active ? null : p)} style={{
              textAlign: 'left',
              padding: '8px 10px',
              background: active ? 'var(--bg-4)' : 'var(--bg-3)',
              border: `1px solid ${active ? 'var(--sig-green)' : 'var(--line-2)'}`,
              borderRadius: 4,
            }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 5 }}>
                <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)' }}>TRACE {i + 1}</span>
                <div style={{ flex: 1 }}/>
                <span className="mono" style={{ fontSize: 10, padding: '1px 6px',
                  color: tone, background: 'var(--bg-2)',
                  border: `1px solid ${tone}`, borderRadius: 3, opacity: 0.85,
                }}>{outcome}</span>
              </div>
              <div className="mono" style={{ fontSize: 10.5, color: 'var(--ink-1)', lineHeight: 1.6 }}>
                {p.map((step, j) => (
                  <React.Fragment key={j}>
                    <span style={{ color: j === 0 ? 'var(--sig-green)' : 'var(--ink-1)' }}>{step}</span>
                    {j < p.length - 1 && <span style={{ color: 'var(--ink-3)' }}> → </span>}
                  </React.Fragment>
                ))}
              </div>
            </button>
          );
        })}
      </div>
    </div>
  );
}

// ── Tab views: steps / vars / source / validation ─────────────────
function StepsTable({ formula, onSelect }) {
  return (
    <div style={{ height: '100%', overflow: 'auto', padding: '14px 22px 30px' }}>
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ borderBottom: '1px solid var(--line-2)' }}>
            {["NAME","KIND","ACTION","WORKSPACE","WHEN","NEEDS","FLAGS"].map(h => (
              <th key={h} className="mono" style={{
                textAlign: 'left', padding: '8px 12px',
                fontSize: 9.5, fontWeight: 600, letterSpacing: 0.6, color: 'var(--ink-3)',
              }}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {formula.steps.map(s => {
            const meta = KIND_META[s.kind] || KIND_META.op;
            return (
              <tr key={s.name}
                onClick={() => onSelect(s.name)}
                style={{ borderBottom: '1px solid var(--line-1)', cursor: 'pointer' }}
                onMouseEnter={e => e.currentTarget.style.background = 'var(--bg-2)'}
                onMouseLeave={e => e.currentTarget.style.background = 'transparent'}
              >
                <td style={{ padding: '8px 12px' }}>
                  <span className="mono" style={{ fontSize: 12, color: 'var(--ink-0)', fontWeight: 600 }}>{s.name}</span>
                  {s.name === formula.entry && <span className="mono" style={{ fontSize: 9.5, color: 'var(--sig-green)', marginLeft: 6 }}>· entry</span>}
                </td>
                <td style={{ padding: '8px 12px' }}>
                  <span className="mono" style={{
                    fontSize: 9.5, padding: '1px 6px', letterSpacing: 0.5, fontWeight: 700,
                    color: meta.fg, background: meta.bg,
                    border: `1px solid ${meta.ring}`, borderRadius: 3,
                  }}>{meta.label}</span>
                </td>
                <td style={{ padding: '8px 12px' }}>
                  <code className="mono" style={{ fontSize: 11, color: 'var(--sig-cyan)' }}>{s.action}</code>
                </td>
                <td style={{ padding: '8px 12px' }}>
                  {s.workspace ? <code className="mono" style={{ fontSize: 11, color: 'var(--sig-amber)' }}>{s.workspace}</code> : <span style={{ color: 'var(--ink-4)' }}>—</span>}
                </td>
                <td style={{ padding: '8px 12px' }}>
                  {s.when ? <code className="mono" style={{ fontSize: 11, color: 'var(--sig-amber)' }}>{truncateExpr(s.when, 28)}</code> : <span style={{ color: 'var(--ink-4)' }}>—</span>}
                </td>
                <td style={{ padding: '8px 12px' }}>
                  <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>
                    {(s.needs || []).join(", ") || "—"}
                  </span>
                </td>
                <td style={{ padding: '8px 12px' }}>
                  <div style={{ display: 'flex', gap: 4 }}>
                    {s.terminal && <Tag color="var(--ink-3)">terminal</Tag>}
                    {s.resets   && s.resets.length > 0 && <Tag color="var(--sig-amber)">resets</Tag>}
                    {s.role     && <Tag color={ROLE_COLOR[s.role]?.fg || 'var(--ink-2)'}>{s.role}</Tag>}
                    {s.verdict_only && <Tag color="var(--sig-purple)">verdict</Tag>}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function Tag({ color, children }) {
  return (
    <span className="mono" style={{
      fontSize: 9.5, padding: '1px 5px',
      color, background: 'var(--bg-3)',
      border: `1px solid ${color}`, opacity: 0.85,
      borderRadius: 3,
    }}>{children}</span>
  );
}

function VarsTable({ formula }) {
  return (
    <div style={{ height: '100%', overflow: 'auto', padding: '14px 22px 30px' }}>
      <SectionHeader>VARIABLES</SectionHeader>
      <table style={{ width: '100%', borderCollapse: 'collapse', marginBottom: 28 }}>
        <thead><tr style={{ borderBottom: '1px solid var(--line-2)' }}>
          {["NAME","TYPE","REQUIRED","DEFAULT","DESCRIPTION"].map(h => (
            <th key={h} className="mono" style={{ textAlign: 'left', padding: '8px 12px',
              fontSize: 9.5, fontWeight: 600, letterSpacing: 0.6, color: 'var(--ink-3)' }}>{h}</th>
          ))}
        </tr></thead>
        <tbody>
          {(formula.vars || []).map(v => (
            <tr key={v.name} style={{ borderBottom: '1px solid var(--line-1)' }}>
              <td style={{ padding: '8px 12px' }}>
                <code className="mono" style={{ fontSize: 12, color: 'var(--ink-0)', fontWeight: 600 }}>{v.name}</code>
              </td>
              <td style={{ padding: '8px 12px' }}>
                <span className="mono" style={{ fontSize: 11, color: 'var(--sig-cyan)' }}>{v.type || "string"}</span>
              </td>
              <td style={{ padding: '8px 12px' }}>
                {v.required
                  ? <span className="mono" style={{ fontSize: 10.5, color: 'var(--sig-red)' }}>required</span>
                  : <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>optional</span>}
              </td>
              <td style={{ padding: '8px 12px' }}>
                {v.default != null
                  ? <code className="mono" style={{ fontSize: 11, color: 'var(--ink-1)' }}>{String(v.default)}</code>
                  : <span style={{ color: 'var(--ink-4)' }}>—</span>}
              </td>
              <td style={{ padding: '8px 12px', fontSize: 12, color: 'var(--ink-2)', maxWidth: 480 }}>
                {v.description || <span style={{ color: 'var(--ink-4)' }}>—</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {(formula.workspaces || []).length > 0 && (
        <>
          <SectionHeader>WORKSPACES</SectionHeader>
          <table style={{ width: '100%', borderCollapse: 'collapse', marginBottom: 28 }}>
            <thead><tr style={{ borderBottom: '1px solid var(--line-2)' }}>
              {["NAME","KIND","BRANCH","BASE","SCOPE","CLEANUP"].map(h => (
                <th key={h} className="mono" style={{ textAlign: 'left', padding: '8px 12px',
                  fontSize: 9.5, fontWeight: 600, letterSpacing: 0.6, color: 'var(--ink-3)' }}>{h}</th>
              ))}
            </tr></thead>
            <tbody>
              {formula.workspaces.map(w => (
                <tr key={w.name} style={{ borderBottom: '1px solid var(--line-1)' }}>
                  <td style={{ padding: '8px 12px' }}>
                    <code className="mono" style={{ fontSize: 12, color: 'var(--sig-amber)', fontWeight: 600 }}>{w.name}</code>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <span className="mono" style={{ fontSize: 11, color: 'var(--ink-1)' }}>{w.kind}</span>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <code className="mono" style={{ fontSize: 11, color: 'var(--sig-blue)' }}>{w.branch}</code>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <code className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{w.base || "—"}</code>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{w.scope || "—"}</span>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{w.cleanup || "—"}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {(formula.outputs || []).length > 0 && (
        <>
          <SectionHeader>OUTPUTS</SectionHeader>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead><tr style={{ borderBottom: '1px solid var(--line-2)' }}>
              {["NAME","TYPE","VALUES","DESCRIPTION"].map(h => (
                <th key={h} className="mono" style={{ textAlign: 'left', padding: '8px 12px',
                  fontSize: 9.5, fontWeight: 600, letterSpacing: 0.6, color: 'var(--ink-3)' }}>{h}</th>
              ))}
            </tr></thead>
            <tbody>
              {formula.outputs.map(o => (
                <tr key={o.name} style={{ borderBottom: '1px solid var(--line-1)' }}>
                  <td style={{ padding: '8px 12px' }}>
                    <code className="mono" style={{ fontSize: 12, color: 'var(--ink-0)', fontWeight: 600 }}>{o.name}</code>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    <span className="mono" style={{ fontSize: 11, color: 'var(--sig-cyan)' }}>{o.type}</span>
                  </td>
                  <td style={{ padding: '8px 12px' }}>
                    {o.values
                      ? <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>[{o.values.join(", ")}]</span>
                      : <span style={{ color: 'var(--ink-4)' }}>—</span>}
                  </td>
                  <td style={{ padding: '8px 12px', fontSize: 12, color: 'var(--ink-2)' }}>
                    {o.description || <span style={{ color: 'var(--ink-4)' }}>—</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}

function SectionHeader({ children }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
      <span className="mono" style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.6, color: 'var(--ink-1)' }}>{children}</span>
      <div style={{ flex: 1, height: 1, background: 'var(--line-2)' }}/>
    </div>
  );
}

// Crude TOML rendering — readable enough for "view source"
function SourceView({ formula }) {
  const lines = formulaToToml(formula);
  return (
    <div style={{ height: '100%', overflow: 'auto', padding: '14px 22px 30px', background: 'var(--bg-1)' }}>
      <pre className="mono" style={{
        margin: 0, fontSize: 11.5, lineHeight: 1.65,
        color: 'var(--ink-1)',
      }}>
        {lines.map(([kind, text], i) => (
          <span key={i} style={{ display: 'block', color: tomlColor(kind) }}>{text || '\u00a0'}</span>
        ))}
      </pre>
    </div>
  );
}

function tomlColor(kind) {
  return ({
    comment: 'var(--ink-3)',
    section: 'var(--sig-purple)',
    key:     'var(--ink-1)',
    string:  'var(--sig-green)',
    number:  'var(--sig-amber)',
    bool:    'var(--sig-cyan)',
    plain:   'var(--ink-1)',
  })[kind] || 'var(--ink-1)';
}

function formulaToToml(f) {
  const out = [];
  out.push(["comment", `# Spire formula: ${f.name}.formula.toml`]);
  out.push(["comment", `# ${f.description}`]);
  out.push(["plain", ""]);
  out.push(["key", `name = "${f.name}"`]);
  out.push(["key", `version = ${f.version}`]);
  if ((f.default_for || []).length) out.push(["key", `default_for = [${f.default_for.map(x => `"${x}"`).join(", ")}]`]);
  out.push(["key", `entry = "${f.entry}"`]);
  out.push(["plain", ""]);
  (f.vars || []).forEach(v => {
    out.push(["section", `[[vars]]`]);
    out.push(["key", `name = "${v.name}"`]);
    if (v.type)        out.push(["key", `type = "${v.type}"`]);
    if (v.required)    out.push(["key", `required = true`]);
    if (v.default)     out.push(["key", `default = "${v.default}"`]);
    if (v.description) out.push(["key", `description = "${v.description}"`]);
    out.push(["plain", ""]);
  });
  (f.workspaces || []).forEach(w => {
    out.push(["section", `[[workspaces]]`]);
    Object.entries(w).forEach(([k, v]) => out.push(["key", `${k} = "${v}"`]));
    out.push(["plain", ""]);
  });
  f.steps.forEach(s => {
    out.push(["section", `[[steps]]`]);
    out.push(["key", `name = "${s.name}"`]);
    out.push(["key", `kind = "${s.kind}"`]);
    out.push(["key", `action = "${s.action}"`]);
    if (s.flow)      out.push(["key", `flow = "${s.flow}"`]);
    if (s.graph)     out.push(["key", `graph = "${s.graph}"`]);
    if (s.title)     out.push(["key", `title = "${s.title}"`]);
    if (s.role)      out.push(["key", `role = "${s.role}"`]);
    if (s.model)     out.push(["key", `model = "${s.model}"`]);
    if (s.timeout)   out.push(["key", `timeout = "${s.timeout}"`]);
    if (s.workspace) out.push(["key", `workspace = "${s.workspace}"`]);
    if (s.needs)     out.push(["key", `needs = [${s.needs.map(n => `"${n}"`).join(", ")}]`]);
    if (s.terminal)  out.push(["key", `terminal = true`]);
    if (s.resets)    out.push(["key", `resets = [${s.resets.map(n => `"${n}"`).join(", ")}]`]);
    if (s.when)      out.push(["key", `when = "${s.when}"`]);
    if (s.with) {
      Object.entries(s.with).forEach(([k, v]) => out.push(["key", `with.${k} = "${v}"`]));
    }
    out.push(["plain", ""]);
  });
  return out;
}

function ValidationView({ formula, onSelectStep }) {
  const issues = formula.issues || [];
  // Synthesize structural checks too
  const checks = [
    { ok: !!formula.entry && formula.steps.find(s => s.name === formula.entry),
      label: "entry references a real step" },
    { ok: formula.steps.some(s => s.terminal),
      label: "at least one terminal step" },
    { ok: formula.steps.every(s => (s.needs || []).every(n => formula.steps.find(x => x.name === n))),
      label: "all needs[] reference real steps" },
    { ok: (formula.edges || []).every(e => formula.steps.find(s => s.name === e.from) && formula.steps.find(s => s.name === e.to)),
      label: "all edges connect existing steps" },
    { ok: formula.steps.filter(s => s.kind === "call").every(s => !!s.graph),
      label: "every CALL step names a sub-graph" },
  ];
  const okCount = checks.filter(c => c.ok).length;
  return (
    <div style={{ height: '100%', overflow: 'auto', padding: '14px 22px 30px' }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 14, marginBottom: 16 }}>
        <h2 style={{ margin: 0, fontSize: 14, fontWeight: 600, color: 'var(--ink-0)' }}>Validation</h2>
        <span className="mono" style={{ fontSize: 11, color: 'var(--ink-3)' }}>
          {okCount}/{checks.length} structural checks pass · {issues.length} authored issue{issues.length === 1 ? '' : 's'}
        </span>
      </div>

      <SectionHeader>STRUCTURAL CHECKS</SectionHeader>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4, marginBottom: 22 }}>
        {checks.map((c, i) => (
          <div key={i} style={{
            display: 'flex', alignItems: 'center', gap: 9,
            padding: '6px 10px',
            background: 'var(--bg-3)',
            border: `1px solid ${c.ok ? 'var(--line-2)' : 'rgba(255,93,93,0.30)'}`,
            borderRadius: 3,
          }}>
            <span style={{ color: c.ok ? 'var(--sig-green)' : 'var(--sig-red)' }}>
              {c.ok ? <Icon.check/> : '✕'}
            </span>
            <span style={{ fontSize: 12, color: 'var(--ink-1)' }}>{c.label}</span>
          </div>
        ))}
      </div>

      <SectionHeader>AUTHORED ISSUES</SectionHeader>
      {issues.length === 0 ? (
        <div style={{
          padding: '14px 16px',
          background: 'rgba(79,227,138,0.05)',
          border: '1px dashed rgba(79,227,138,0.20)',
          borderRadius: 4,
          fontSize: 12.5, color: 'var(--ink-2)',
        }}>
          <span className="mono" style={{ color: 'var(--sig-green)' }}>✓</span> No outstanding issues. Last linted on push.
        </div>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {issues.map((iss, i) => {
            const isError = iss.level === "error";
            return (
              <button key={i} onClick={() => onSelectStep(iss.phase)} style={{
                textAlign: 'left',
                padding: '10px 12px',
                background: isError ? 'rgba(255,93,93,0.06)' : 'rgba(247,201,72,0.06)',
                border: `1px solid ${isError ? 'rgba(255,93,93,0.30)' : 'rgba(247,201,72,0.30)'}`,
                borderLeft: `3px solid ${isError ? 'var(--sig-red)' : 'var(--sig-amber)'}`,
                borderRadius: 4,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
                  <span className="mono" style={{
                    fontSize: 10, fontWeight: 700, letterSpacing: 0.5,
                    color: isError ? 'var(--sig-red)' : 'var(--sig-amber)',
                  }}>{iss.level.toUpperCase()}</span>
                  <span className="mono" style={{ fontSize: 11, color: 'var(--ink-1)' }}>step: {iss.phase}</span>
                </div>
                <div style={{ fontSize: 12.5, color: 'var(--ink-1)', lineHeight: 1.5 }}>
                  {iss.message}
                </div>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

Object.assign(window, {
  GraphCanvas, StepInspector, StepsTable, VarsTable, SourceView, ValidationView,
});
