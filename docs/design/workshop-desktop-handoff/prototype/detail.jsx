// Spire Desktop — Bead Detail slide-in panel

function DetailPanel({ bead, onClose, onOpenBead, onOpenFormula }) {
  const [tab, setTab] = React.useState("details");
  React.useEffect(() => { setTab("details"); }, [bead?.id]);
  if (!bead) return null;
  const acts = activityFor(bead.id);
  const blockers = (bead.blockedBy || []).map(id => BEADS.find(b => b.id === id)).filter(Boolean);
  const blocks   = (bead.blocks || []).map(id => BEADS.find(b => b.id === id)).filter(Boolean);
  const subtasks = (bead.subtaskIds || []).map(id => BEADS.find(b => b.id === id)).filter(Boolean);
  const msgs     = MESSAGES.filter(m => m.refBead === bead.id);
  const comments = commentsFor(bead.id);
  const pods     = podsFor(bead.id);

  return (
    <>
      <div onClick={onClose} style={{
        position: 'absolute', inset: 0,
        background: 'rgba(3,5,9,0.55)',
        backdropFilter: 'blur(2px)',
        zIndex: 10,
      }} className="fade-in"/>

      <aside className="slide-in" style={{
        position: 'absolute', top: 0, right: 0, bottom: 0,
        width: '46%', minWidth: 560, maxWidth: 720,
        background: 'var(--bg-1)',
        borderLeft: '1px solid var(--line-3)',
        boxShadow: '-20px 0 40px rgba(0,0,0,0.45)',
        zIndex: 11,
        display: 'flex', flexDirection: 'column',
      }}>
        {/* Panel header */}
        <div style={{
          padding: '14px 18px 12px',
          borderBottom: '1px solid var(--line-2)',
          background: 'var(--bg-2)',
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
            <span className="mono" style={{ fontSize: 12, color: 'var(--ink-2)', fontWeight: 600, letterSpacing: 0.3 }}>
              {bead.id}
            </span>
            <TypeBadge type={bead.type} size="md"/>
            <PriorityDot p={bead.priority} size={8}/>
            <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-3)' }}>{bead.priority}</span>
            <div style={{ flex: 1 }}/>
            <StatusPill status={bead.status}/>
            <button onClick={onClose} style={{
              width: 24, height: 24, display: 'flex', alignItems: 'center', justifyContent: 'center',
              borderRadius: 4, color: 'var(--ink-2)',
            }}><Icon.close/></button>
          </div>
          <h1 style={{
            margin: 0, fontSize: 17, fontWeight: 600, color: 'var(--ink-0)',
            lineHeight: 1.3, letterSpacing: -0.1,
          }}>{bead.title}</h1>
          {bead.parent && (
            <div style={{ marginTop: 7, display: 'flex', alignItems: 'center', gap: 6 }}>
              <Icon.parent style={{ color: 'var(--ink-3)' }}/>
              <span className="mono" style={{ fontSize: 11, color: 'var(--ink-3)' }}>parent:</span>
              <button onClick={() => onOpenBead(BEADS.find(b => b.id === bead.parent))}
                className="mono" style={{ fontSize: 11, color: 'var(--sig-blue)' }}>
                {bead.parent}
              </button>
            </div>
          )}
        </div>

        {/* Tabs */}
        <div style={{
          display: 'flex', gap: 0, padding: '0 18px',
          borderBottom: '1px solid var(--line-2)',
          background: 'var(--bg-1)',
        }}>
          {[
            { id: "details", label: "DETAILS", count: null },
            { id: "comments", label: "COMMENTS", count: comments.length || null },
            { id: "logs", label: "LOGS", count: pods.length || null, badge: pods.some(p => p.status === 'active') },
            { id: "activity", label: "ACTIVITY", count: acts.length || null },
          ].map(t => {
            const active = tab === t.id;
            return (
              <button key={t.id} onClick={() => setTab(t.id)} className="mono" style={{
                display: 'inline-flex', alignItems: 'center', gap: 6,
                padding: '10px 14px', fontSize: 10.5, fontWeight: 600, letterSpacing: 0.5,
                color: active ? 'var(--ink-0)' : 'var(--ink-3)',
                borderBottom: `2px solid ${active ? 'var(--sig-green)' : 'transparent'}`,
                marginBottom: -1,
              }}>
                {t.label}
                {t.count != null && (
                  <span style={{
                    padding: '1px 5px', fontSize: 9.5, borderRadius: 3,
                    background: active ? 'var(--bg-3)' : 'var(--bg-2)',
                    color: active ? 'var(--ink-1)' : 'var(--ink-3)',
                  }}>{t.count}</span>
                )}
                {t.badge && (
                  <span style={{
                    width: 5, height: 5, borderRadius: '50%', background: 'var(--sig-green)',
                    boxShadow: '0 0 4px var(--sig-green)',
                  }} className="pulse-green"/>
                )}
              </button>
            );
          })}
        </div>

        {/* Actions bar */}
        <div style={{
          display: 'flex', gap: 8, padding: '10px 18px',
          borderBottom: '1px solid var(--line-2)',
          background: 'var(--bg-1)',
        }}>
          {bead.status === "open" && <ActionBtn primary icon={<Icon.bolt/>}>Claim</ActionBtn>}
          {bead.status === "open" && <ActionBtn icon={<Icon.check/>}>Mark ready</ActionBtn>}
          {(bead.status === "in_progress" || bead.status === "review") && <ActionBtn icon={<Icon.message/>}>Comment</ActionBtn>}
          {(bead.status === "in_progress" || bead.status === "review") && <ActionBtn icon={<Icon.message/>}>Message agent</ActionBtn>}
          {bead.status !== "closed" && <ActionBtn danger icon={<Icon.close/>}>Close</ActionBtn>}
          <div style={{ flex: 1 }}/>
          <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)', alignSelf: 'center' }}>
            updated {relTimeLong(bead.updated)}
          </span>
        </div>

        {/* Body */}
        <div style={{ flex: 1, overflow: 'auto', padding: '16px 18px 24px' }}>
          {tab === "details" && (
            <>
              {/* Formula lifecycle */}
              <FormulaLifecycle bead={bead} onOpenFormula={onOpenFormula}/>

              {/* Metadata grid */}
              <Section title="Context">
                <MetaGrid bead={bead} onOpenBead={onOpenBead}/>
              </Section>

              {/* Description — markdown */}
              <Section title="Description">
                <div style={{
                  padding: '14px 16px', background: 'var(--bg-2)',
                  border: '1px solid var(--line-2)', borderRadius: 5,
                }}>
                  <Markdown src={bead.description}/>
                </div>
              </Section>

              {/* Dependencies */}
              {(blockers.length > 0 || blocks.length > 0) && (
                <Section title="Dependencies">
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                    {blockers.length > 0 && (
                      <DepGroup label="BLOCKED BY" color="var(--sig-amber)" beads={blockers} onOpen={onOpenBead}/>
                    )}
                    {blocks.length > 0 && (
                      <DepGroup label="BLOCKS" color="var(--sig-blue)" beads={blocks} onOpen={onOpenBead}/>
                    )}
                  </div>
                </Section>
              )}

              {/* Subtasks */}
              {subtasks.length > 0 && (
                <Section title={`Subtasks · ${subtasks.filter(s => s.status === "closed").length}/${subtasks.length} complete`}>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    {subtasks.map(s => (
                      <SubtaskRow key={s.id} bead={s} onOpen={onOpenBead}/>
                    ))}
                  </div>
                </Section>
              )}

              {/* Messages for this bead */}
              {msgs.length > 0 && (
                <Section title="Related messages">
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    {msgs.map(m => (
                      <div key={m.id} style={{
                        padding: '9px 11px',
                        background: 'var(--bg-2)', border: '1px solid var(--line-2)', borderRadius: 5,
                      }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 7, marginBottom: 4 }}>
                          <AgentChip name={m.from} role={m.role}/>
                          <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)' }}>· {relTime(m.time)}</span>
                        </div>
                        <div style={{ fontSize: 12, color: 'var(--ink-0)', fontWeight: 500, marginBottom: 2 }}>{m.subject}</div>
                        <div style={{ fontSize: 11.5, color: 'var(--ink-2)', lineHeight: 1.5 }}>{m.preview}</div>
                      </div>
                    ))}
                  </div>
                </Section>
              )}

              {/* Run stats (if any) */}
              {bead.runs > 0 && (
                <Section title="Runs">
                  <RunStats bead={bead}/>
                </Section>
              )}
            </>
          )}

          {tab === "comments" && (
            <CommentsThread beadId={bead.id} onOpenBead={onOpenBead}/>
          )}

          {tab === "logs" && (
            <LogViewer beadId={bead.id}/>
          )}

          {tab === "activity" && (
            <Section title="Activity">
              <Timeline items={acts}/>
            </Section>
          )}
        </div>
      </aside>
    </>
  );
}

function Section({ title, children }) {
  return (
    <div style={{ marginBottom: 20 }}>
      <div className="mono" style={{
        fontSize: 10, fontWeight: 700, letterSpacing: 0.7,
        color: 'var(--ink-3)', marginBottom: 8, textTransform: 'uppercase',
      }}>{title}</div>
      {children}
    </div>
  );
}

function ActionBtn({ children, icon, primary, danger }) {
  const fg = primary ? '#0b0e14' : danger ? 'var(--sig-red)' : 'var(--ink-1)';
  const bg = primary ? 'var(--sig-green)' : 'var(--bg-3)';
  const border = primary ? 'var(--sig-green)' : danger ? 'rgba(255,93,93,0.25)' : 'var(--line-3)';
  return (
    <button style={{
      display: 'inline-flex', alignItems: 'center', gap: 6,
      height: 28, padding: '0 11px',
      fontSize: 11.5, fontWeight: 600, letterSpacing: 0.3,
      color: fg, background: bg,
      border: `1px solid ${border}`, borderRadius: 4,
    }}>
      {icon}{children}
    </button>
  );
}

// Map bead.type → formula name in Workshop. Recovery beads run cleric-default;
// design beads have no dedicated formula yet so they fall back to task-default.
const FORMULA_FOR_TYPE = {
  task:     "task-default",
  feature:  "task-default",
  bug:      "bug-default",
  chore:    "chore-default",
  epic:     "epic-default",
  recovery: "cleric-default",
  design:   "task-default",
};

function FormulaLifecycle({ bead, onOpenFormula }) {
  const formulaName = FORMULA_FOR_TYPE[bead.type] || "task-default";
  // Per-formula step lists. Keep this in sync with workshop-data.jsx
  // (we only show the linear backbone here — terminals fan off the last step).
  const STEPS_BY_FORMULA = {
    "task-default":  ["plan", "implement", "review", "merge"],
    "bug-default":   ["plan", "implement", "review", "merge"],
    "chore-default": ["research", "implement", "document", "review", "merge"],
    "epic-default":  ["design-check", "plan", "materialize", "implement", "review", "merge"],
    "cleric-default":["decide", "execute", "verify", "learn", "close"],
  };
  const steps = STEPS_BY_FORMULA[formulaName] || STEPS_BY_FORMULA["task-default"];
  // infer current step
  let current = 0;
  if (bead.status === "closed") current = steps.length;
  else if (bead.phase && bead.phase.includes("review"))   current = steps.indexOf("review");
  else if (bead.phase && bead.phase.includes("implement")) current = steps.indexOf("implement");
  else if (bead.phase && bead.phase.includes("plan"))     current = steps.indexOf("plan");
  else if (bead.phase && bead.phase.includes("validate-design")) current = 0;
  if (current < 0) current = 0;

  return (
    <div style={{ marginBottom: 20 }}>
      <FormulaPill name={formulaName} onClick={onOpenFormula ? () => onOpenFormula(formulaName) : null}/>
      <div style={{
        display: 'flex', alignItems: 'stretch', gap: 0,
        background: 'var(--bg-2)', border: '1px solid var(--line-2)',
        borderRadius: 5, overflow: 'hidden',
      }}>
        {steps.map((s, i) => {
          const done    = i < current;
          const active  = i === current && bead.status !== "closed";
          const pending = i > current;
          const color = done ? 'var(--sig-mint)' : active ? 'var(--sig-green)' : 'var(--ink-3)';
          return (
            <div key={s} style={{
              flex: 1, padding: '9px 8px 8px',
              borderRight: i < steps.length - 1 ? '1px solid var(--line-2)' : 'none',
              position: 'relative',
              background: active ? 'rgba(79,227,138,0.05)' : 'transparent',
            }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3 }}>
                <span style={{
                  width: 6, height: 6, borderRadius: '50%', background: color,
                  boxShadow: active ? `0 0 6px ${color}` : 'none',
                  opacity: pending ? 0.4 : 1,
                }} className={active ? "pulse-green" : ""}/>
                <span className="mono" style={{
                  fontSize: 10, fontWeight: 600, letterSpacing: 0.5, color,
                  textTransform: 'uppercase', opacity: pending ? 0.55 : 1,
                }}>{s}</span>
              </div>
              {done && <div className="mono" style={{ fontSize: 9.5, color: 'var(--ink-3)' }}>✓ complete</div>}
              {active && <div className="mono" style={{ fontSize: 9.5, color: 'var(--sig-green)' }}>in flight…</div>}
              {pending && <div className="mono" style={{ fontSize: 9.5, color: 'var(--ink-4)' }}>queued</div>}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function MetaGrid({ bead, onOpenBead }) {
  const rows = [
    { k: "ASSIGNEE",  v: bead.assignee ? <AgentChip name={bead.assignee} role={bead.role} active={bead.status !== "closed"}/> : <span className="mono" style={{fontSize: 11, color:'var(--ink-3)', fontStyle:'italic'}}>unclaimed</span> },
    { k: "PHASE",     v: <PhaseLabel phase={bead.phase}/> },
    { k: "PRIORITY",  v: <span className="mono" style={{ fontSize: 11, color: PRIORITY_COLOR[bead.priority] }}>{bead.priority} · {PRIORITY_LABEL[bead.priority]}</span> },
    { k: "BRANCH",    v: bead.branch ? <span className="mono" style={{ fontSize: 11, color: 'var(--ink-1)' }}>{bead.branch}</span> : <span className="mono" style={{fontSize: 11, color:'var(--ink-3)'}}>—</span> },
    { k: "REPO",      v: <span className="mono" style={{ fontSize: 11, color: 'var(--ink-1)' }}>{bead.repo}</span> },
    { k: "FILED",     v: <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{relTimeLong(bead.created)}</span> },
  ];
  return (
    <div style={{
      display: 'grid', gridTemplateColumns: '1fr 1fr',
      background: 'var(--bg-2)', border: '1px solid var(--line-2)', borderRadius: 5,
    }}>
      {rows.map((r, i) => (
        <div key={r.k} style={{
          padding: '9px 12px',
          borderBottom: i < rows.length - 2 ? '1px solid var(--line-1)' : 'none',
          borderRight: i % 2 === 0 ? '1px solid var(--line-1)' : 'none',
          display: 'flex', flexDirection: 'column', gap: 3,
        }}>
          <span className="mono" style={{ fontSize: 9.5, color: 'var(--ink-3)', letterSpacing: 0.6 }}>{r.k}</span>
          <div>{r.v}</div>
        </div>
      ))}
    </div>
  );
}

function PhaseLabel({ phase }) {
  if (!phase || phase === "queued") return <span className="mono" style={{ fontSize: 11, color: 'var(--ink-3)' }}>queued</span>;
  if (phase === "merged") return <span className="mono" style={{ fontSize: 11, color: 'var(--sig-mint)' }}>merged</span>;
  const color = phase.includes("review") ? 'var(--sig-purple)'
    : phase.includes("plan") ? 'var(--sig-cyan)'
    : phase.includes("implement") ? 'var(--sig-green)'
    : phase.includes("diagnos") ? 'var(--sig-amber)'
    : 'var(--ink-1)';
  return (
    <span className="mono" style={{ display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 11, color }}>
      <span className="blink" style={{ width: 4, height: 4, borderRadius: '50%', background: color }}/>
      {phase}
    </span>
  );
}

function DepGroup({ label, color, beads, onOpen }) {
  return (
    <div>
      <div className="mono" style={{ fontSize: 9.5, color, letterSpacing: 0.6, marginBottom: 5 }}>{label}</div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
        {beads.map(b => (
          <button key={b.id} onClick={() => onOpen(b)} style={{
            display: 'flex', alignItems: 'center', gap: 8,
            padding: '7px 10px', textAlign: 'left',
            background: 'var(--bg-2)', border: '1px solid var(--line-2)', borderRadius: 4,
          }}>
            <Icon.dep style={{ color }}/>
            <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)' }}>{b.id}</span>
            <PriorityDot p={b.priority} size={5}/>
            <span style={{ fontSize: 11.5, color: 'var(--ink-1)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{b.title}</span>
            <div style={{ flex: 1 }}/>
            <StatusPill status={b.status}/>
          </button>
        ))}
      </div>
    </div>
  );
}

function SubtaskRow({ bead, onOpen }) {
  return (
    <button onClick={() => onOpen(bead)} style={{
      display: 'flex', alignItems: 'center', gap: 8,
      padding: '8px 10px', textAlign: 'left',
      background: 'var(--bg-2)', border: '1px solid var(--line-2)', borderRadius: 4,
    }}>
      <span className="mono" style={{ fontSize: 11, color: 'var(--ink-2)', width: 80 }}>{bead.id}</span>
      <PriorityDot p={bead.priority} size={6}/>
      <TypeBadge type={bead.type}/>
      <span style={{ flex: 1, fontSize: 12, color: 'var(--ink-1)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{bead.title}</span>
      {bead.assignee && <AgentChip name={bead.assignee} role={bead.role} active={bead.status !== "closed"}/>}
      <StatusPill status={bead.status}/>
    </button>
  );
}

function Timeline({ items }) {
  const kindColor = {
    phase: 'var(--sig-green)', dispatch: 'var(--sig-cyan)', status: 'var(--sig-amber)',
    comment: 'var(--ink-2)', merge: 'var(--sig-mint)', plan: 'var(--sig-cyan)',
    claim: 'var(--sig-blue)', file: 'var(--ink-2)', review: 'var(--sig-purple)',
    implement: 'var(--sig-green)',
  };
  return (
    <div style={{ display: 'flex', flexDirection: 'column', position: 'relative', paddingLeft: 14 }}>
      <div style={{ position: 'absolute', left: 5, top: 6, bottom: 6, width: 1, background: 'var(--line-2)' }}/>
      {items.map((a, i) => (
        <div key={i} style={{ display: 'flex', alignItems: 'flex-start', gap: 10, padding: '5px 0', position: 'relative' }}>
          <span style={{
            position: 'absolute', left: -14, top: 8,
            width: 9, height: 9, borderRadius: '50%',
            background: 'var(--bg-1)',
            border: `1.5px solid ${kindColor[a.kind] || 'var(--ink-2)'}`,
          }}/>
          <span className="mono" style={{ fontSize: 10, color: 'var(--ink-3)', width: 54, flexShrink: 0 }}>{relTime(a.time)}</span>
          <span className="mono" style={{ fontSize: 10, color: kindColor[a.kind] || 'var(--ink-2)', width: 70, flexShrink: 0, letterSpacing: 0.4 }}>
            {a.kind.toUpperCase()}
          </span>
          <span style={{ fontSize: 12, color: 'var(--ink-1)', flex: 1 }}>{a.text}</span>
        </div>
      ))}
    </div>
  );
}

function RunStats({ bead }) {
  return (
    <div style={{
      display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)',
      background: 'var(--bg-2)', border: '1px solid var(--line-2)', borderRadius: 5,
    }}>
      {[
        { k: "RUNS", v: bead.runs },
        { k: "SUCCESS", v: bead.successRate != null ? `${Math.round(bead.successRate * 100)}%` : "—" },
        { k: "COST", v: `$${bead.cost.toFixed(2)}` },
        { k: "ROUNDS", v: bead.reviewRound || 0 },
      ].map((s, i) => (
        <div key={s.k} style={{
          padding: '10px 12px',
          borderRight: i < 3 ? '1px solid var(--line-1)' : 'none',
          display: 'flex', flexDirection: 'column', gap: 2,
        }}>
          <span className="mono" style={{ fontSize: 9.5, color: 'var(--ink-3)', letterSpacing: 0.6 }}>{s.k}</span>
          <span style={{ fontSize: 16, fontWeight: 600, color: 'var(--ink-0)' }} className="mono">{s.v}</span>
        </div>
      ))}
    </div>
  );
}

function FormulaPill({ name, onClick }) {
  const [hover, setHover] = React.useState(false);
  const interactive = typeof onClick === 'function';
  const accent = hover && interactive ? 'var(--sig-green)' : 'var(--ink-3)';
  const border = hover && interactive ? 'var(--sig-green)' : 'var(--line-2)';
  const bg     = hover && interactive ? 'rgba(79,227,138,0.06)' : 'var(--bg-2)';

  return (
    <button
      type="button"
      onClick={interactive ? onClick : undefined}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      disabled={!interactive}
      className="mono"
      title={interactive ? `Open ${name} in Workshop` : null}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 8,
        padding: '5px 9px 5px 10px',
        marginBottom: 10,
        background: bg,
        border: `1px solid ${border}`,
        borderRadius: 4,
        color: accent,
        fontSize: 10, fontWeight: 700, letterSpacing: 0.7,
        cursor: interactive ? 'pointer' : 'default',
        boxShadow: hover && interactive ? '0 0 0 3px rgba(79,227,138,0.10)' : 'none',
        transition: 'background 120ms ease, border-color 120ms ease, color 120ms ease, box-shadow 120ms ease',
      }}>
      <span style={{ color: 'var(--ink-3)' }}>FORMULA</span>
      <span style={{ color: 'var(--ink-4)' }}>·</span>
      <span style={{ color: hover && interactive ? 'var(--sig-green)' : 'var(--ink-1)' }}>{name}</span>
      {interactive && (
        <svg width="11" height="11" viewBox="0 0 16 16" fill="none" style={{
          marginLeft: 2,
          opacity: hover ? 1 : 0.6,
          transform: hover ? 'translate(1px,-1px)' : 'translate(0,0)',
          transition: 'transform 120ms ease, opacity 120ms ease',
        }}>
          <path d="M5 11L11 5M11 5H6M11 5V10" stroke="currentColor" strokeWidth="1.5"
                strokeLinecap="round" strokeLinejoin="round"/>
        </svg>
      )}
    </button>
  );
}

Object.assign(window, { DetailPanel });
