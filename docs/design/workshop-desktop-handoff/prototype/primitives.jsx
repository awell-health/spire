// Spire Desktop — shared primitives: badges, icons, formatters

const PRIORITY_COLOR = {
  P0: "var(--sig-red)", P1: "var(--sig-orange)",
  P2: "var(--sig-amber)", P3: "var(--sig-blue)", P4: "var(--sig-slate)",
};
const PRIORITY_LABEL = {
  P0: "critical", P1: "high", P2: "med", P3: "low", P4: "nice",
};
const STATUS_COLOR = {
  open: "var(--ink-2)",
  in_progress: "var(--sig-green)",
  review: "var(--sig-purple)",
  closed: "var(--ink-3)",
  hooked: "var(--sig-amber)",
};
const STATUS_LABEL = {
  open: "OPEN", in_progress: "IN PROGRESS",
  review: "REVIEW", closed: "CLOSED", hooked: "HOOKED",
};
const TYPE_COLOR = {
  task:     { bg: "rgba(90,165,255,0.10)",  fg: "#86b9ff", ring: "rgba(90,165,255,0.25)" },
  bug:      { bg: "rgba(255,93,93,0.10)",   fg: "#ff8b8b", ring: "rgba(255,93,93,0.25)" },
  feature:  { bg: "rgba(43,217,161,0.10)",  fg: "#4fe3a7", ring: "rgba(43,217,161,0.25)" },
  epic:     { bg: "rgba(179,140,255,0.12)", fg: "#c8adff", ring: "rgba(179,140,255,0.30)" },
  chore:    { bg: "rgba(110,119,147,0.15)", fg: "#a2aac1", ring: "rgba(110,119,147,0.30)" },
  design:   { bg: "rgba(92,225,230,0.10)",  fg: "#7ee8ec", ring: "rgba(92,225,230,0.25)" },
  recovery: { bg: "rgba(247,201,72,0.12)",  fg: "#f7d46a", ring: "rgba(247,201,72,0.30)" },
};
const ROLE_COLOR = {
  steward:    { fg: "#c8adff", bg: "rgba(179,140,255,0.10)", ring: "rgba(179,140,255,0.25)" },
  wizard:     { fg: "#7ee8ec", bg: "rgba(92,225,230,0.10)",  ring: "rgba(92,225,230,0.25)" },
  apprentice: { fg: "#86b9ff", bg: "rgba(90,165,255,0.10)",  ring: "rgba(90,165,255,0.25)" },
  sage:       { fg: "#ff9fd7", bg: "rgba(255,123,212,0.10)", ring: "rgba(255,123,212,0.25)" },
  arbiter:    { fg: "#f7d46a", bg: "rgba(247,201,72,0.10)",  ring: "rgba(247,201,72,0.25)" },
  cleric:     { fg: "#4fe3a7", bg: "rgba(43,217,161,0.10)",  ring: "rgba(43,217,161,0.25)" },
  artificer:  { fg: "#ffb080", bg: "rgba(255,152,73,0.10)",  ring: "rgba(255,152,73,0.25)" },
};

// ── formatters ─────────────────────────────────────────────
function relTime(ts) {
  const d = Math.max(0, (Date.now() - ts) / 1000);
  if (d < 5) return "just now";
  if (d < 60) return `${Math.floor(d)}s`;
  if (d < 3600) return `${Math.floor(d / 60)}m`;
  if (d < 86400) return `${Math.floor(d / 3600)}h`;
  return `${Math.floor(d / 86400)}d`;
}
function relTimeLong(ts) {
  const d = Math.max(0, (Date.now() - ts) / 1000);
  if (d < 60) return `${Math.floor(d)} sec ago`;
  if (d < 3600) return `${Math.floor(d / 60)} min ago`;
  if (d < 86400) return `${Math.floor(d / 3600)} hr ago`;
  return `${Math.floor(d / 86400)} days ago`;
}
function timeOfDay(ts) {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

// ── atoms ──────────────────────────────────────────────────

function PriorityDot({ p, size = 8 }) {
  return (
    <span
      title={`${p} · ${PRIORITY_LABEL[p]}`}
      style={{
        display: 'inline-block', width: size, height: size, borderRadius: '50%',
        background: PRIORITY_COLOR[p],
        boxShadow: p === 'P0' ? '0 0 6px ' + PRIORITY_COLOR[p] : 'none',
        flexShrink: 0,
      }}
    />
  );
}

function TypeBadge({ type, size = "sm" }) {
  const c = TYPE_COLOR[type] || TYPE_COLOR.task;
  const padding = size === "sm" ? "2px 6px" : "3px 8px";
  const fs = size === "sm" ? 10 : 11;
  return (
    <span className="mono" style={{
      padding, fontSize: fs, fontWeight: 600, letterSpacing: 0.4,
      color: c.fg, background: c.bg, border: `1px solid ${c.ring}`,
      borderRadius: 3, textTransform: 'uppercase',
    }}>
      {type}
    </span>
  );
}

function RoleBadge({ role, size = "sm" }) {
  const c = ROLE_COLOR[role] || ROLE_COLOR.wizard;
  const padding = size === "sm" ? "1px 5px" : "2px 7px";
  const fs = size === "sm" ? 9.5 : 10.5;
  return (
    <span className="mono" style={{
      padding, fontSize: fs, fontWeight: 600, letterSpacing: 0.5,
      color: c.fg, background: c.bg, border: `1px solid ${c.ring}`,
      borderRadius: 3, textTransform: 'uppercase',
    }}>
      {role}
    </span>
  );
}

function StatusPill({ status }) {
  const color = STATUS_COLOR[status];
  const active = status === "in_progress";
  const review = status === "review";
  return (
    <span className="mono" style={{
      display: 'inline-flex', alignItems: 'center', gap: 6,
      padding: '3px 8px', fontSize: 10, fontWeight: 600, letterSpacing: 0.6,
      color, background: 'transparent',
      border: `1px solid ${color}`,
      borderRadius: 3,
    }}>
      <span style={{
        width: 6, height: 6, borderRadius: '50%', background: color,
        boxShadow: active ? '0 0 6px ' + color : review ? '0 0 4px ' + color : 'none',
      }} className={active ? "pulse-green" : ""} />
      {STATUS_LABEL[status]}
    </span>
  );
}

function AgentChip({ name, role, active, compact }) {
  const c = ROLE_COLOR[role] || ROLE_COLOR.wizard;
  return (
    <span className="mono" style={{
      display: 'inline-flex', alignItems: 'center', gap: 6,
      fontSize: compact ? 11 : 11.5, color: c.fg,
    }}>
      <span style={{
        width: 6, height: 6, borderRadius: '50%', background: c.fg,
        boxShadow: active ? '0 0 5px ' + c.fg : 'none',
      }} className={active ? "pulse-green" : ""} />
      {name}
    </span>
  );
}

// ── icons (inline svg) ─────────────────────────────────────
const Icon = {
  search: (p) => <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>,
  plus:   (p) => <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M12 5v14M5 12h14"/></svg>,
  close:  (p) => <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M18 6 6 18M6 6l12 12"/></svg>,
  chevR:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" {...p}><path d="m9 6 6 6-6 6"/></svg>,
  chevD:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" {...p}><path d="m6 9 6 6 6-6"/></svg>,
  board:  (p) => <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><rect x="3" y="4" width="5" height="16" rx="1"/><rect x="10" y="4" width="5" height="11" rx="1"/><rect x="17" y="4" width="4" height="7" rx="1"/></svg>,
  inbox:  (p) => <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11Z"/></svg>,
  roster: (p) => <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><circle cx="9" cy="8" r="3"/><circle cx="17" cy="10" r="2.5"/><path d="M3 20c0-3 3-5 6-5s6 2 6 5"/><path d="M15 20c0-2 2-3.5 4-3.5"/></svg>,
  graph:  (p) => <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><circle cx="5" cy="6" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="12" cy="18" r="2"/><path d="M7 6h10M6 8l5 8M18 8l-5 8"/></svg>,
  metrics:(p) => <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><path d="M3 20h18M6 16v-5M11 16V8M16 16v-7M20 16v-3"/></svg>,
  workshop:(p)=> <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><circle cx="6" cy="6" r="2"/><circle cx="18" cy="6" r="2"/><circle cx="12" cy="14" r="2"/><circle cx="6" cy="20" r="2"/><circle cx="18" cy="20" r="2"/><path d="M8 6h8M7.5 7.5 11 13M16.5 7.5 13 13M10.5 15.5 7 19M13.5 15.5 17 19"/></svg>,
  settings:(p)=> <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" {...p}><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9c.36.13.67.35.91.6s.42.55.51.88H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>,
  link:   (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg>,
  check:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" {...p}><path d="M20 6 9 17l-5-5"/></svg>,
  merge:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M6 21V9a9 9 0 0 0 9 9"/></svg>,
  bolt:   (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg>,
  parent: (p) => <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M9 3v10a4 4 0 0 0 4 4h8"/><path d="m17 13 4 4-4 4"/></svg>,
  dep:    (p) => <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M3 12h7"/><path d="m7 8 4 4-4 4"/><circle cx="17" cy="12" r="3"/></svg>,
  activity:(p)=> <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>,
  dot:    (p) => <svg width="4" height="4" viewBox="0 0 4 4" {...p}><circle cx="2" cy="2" r="2" fill="currentColor"/></svg>,
  spark:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M12 3v3M12 18v3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M3 12h3M18 12h3M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1"/></svg>,
  message:(p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>,
  tri:    (p) => <svg width="8" height="8" viewBox="0 0 8 8" {...p}><path d="M2 1 6 4 2 7z" fill="currentColor"/></svg>,
  triDown:(p) => <svg width="8" height="8" viewBox="0 0 8 8" {...p}><path d="M1 2 7 2 4 6z" fill="currentColor"/></svg>,
  clock:  (p) => <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>,
  wifi:   (p) => <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" {...p}><path d="M5 12.55a11 11 0 0 1 14 0"/><path d="M1.42 9a16 16 0 0 1 21.16 0"/><path d="M8.53 16.11a6 6 0 0 1 6.95 0"/><circle cx="12" cy="20" r="1"/></svg>,
};

// tower logo — stylized obsidian tower mark
function SpireMark({ size = 22, glow = true }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" fill="none">
      <defs>
        <linearGradient id="spireGrad" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#6edd9c"/>
          <stop offset="100%" stopColor="#2a8f5e"/>
        </linearGradient>
      </defs>
      <path d="M16 2 L26 12 L22 30 L10 30 L6 12 Z" fill="url(#spireGrad)" opacity="0.85"/>
      <path d="M16 2 L26 12 L22 30 L10 30 L6 12 Z" stroke="#8af0b4" strokeWidth="0.7" fill="none" opacity="0.8"/>
      <path d="M16 2 L16 30" stroke="#0b0e14" strokeWidth="0.5"/>
      <path d="M6 12 L26 12" stroke="#0b0e14" strokeWidth="0.5"/>
      <circle cx="16" cy="12" r="1.5" fill="#0b0e14"/>
      {glow && <circle cx="16" cy="12" r="1.5" fill="#8af0b4" opacity="0.9"/>}
    </svg>
  );
}

Object.assign(window, {
  PRIORITY_COLOR, PRIORITY_LABEL, STATUS_COLOR, STATUS_LABEL, TYPE_COLOR, ROLE_COLOR,
  relTime, relTimeLong, timeOfDay,
  PriorityDot, TypeBadge, RoleBadge, StatusPill, AgentChip, Icon, SpireMark,
});
