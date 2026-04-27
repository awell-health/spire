// Spire Desktop — main app
// Wires chrome + views + detail panel + quick-file modal + keyboard shortcuts

function App() {
  const [view, setView]           = React.useState("board");
  const [search, setSearch]       = React.useState("");
  const [selectedBead, setBead]   = React.useState(null);
  const [quickFileOpen, setQF]    = React.useState(false);
  const [selectedMsg, setMsg]     = React.useState(MESSAGES[0].id);
  const [expandedEpics, setEpics] = React.useState(new Set(["spi-a3f8", "spi-c7d2"]));
  const [filter]                  = React.useState({ priority: "all", type: "all" });
  const [connection, setConn]     = React.useState(TOWER.connection);
  const [workshopFormula, setWorkshopFormula] = React.useState("task-default");
  const [theme, setTheme]         = React.useState(
    (typeof document !== 'undefined' && document.documentElement.getAttribute('data-theme')) || 'dark'
  );

  React.useEffect(() => {
    if (theme === 'dark') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);
  const [tick, setTick]           = React.useState(0); // forces re-render for live time

  // re-render every 10s so relTime stays fresh
  React.useEffect(() => {
    const t = setInterval(() => setTick(n => n + 1), 10000);
    return () => clearInterval(t);
  }, []);

  // global shortcuts
  React.useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === "n" || e.key === "N")) {
        e.preventDefault();
        setQF(true);
      }
      if ((e.metaKey || e.ctrlKey) && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        document.querySelector('input[placeholder*="Search"]')?.focus();
      }
      if (e.key === "Escape") {
        if (quickFileOpen) setQF(false);
        else if (selectedBead) setBead(null);
      }
      if (!e.metaKey && !e.ctrlKey && e.target?.tagName !== "INPUT" && e.target?.tagName !== "TEXTAREA") {
        // When in graph view, "1" and "2" are reserved for HIERARCHY/LINEAGE mode toggles.
        if (view !== "graph") {
          if (e.key === "1") setView("board");
          if (e.key === "2") setView("inbox");
        }
        if (e.key === "3") setView("roster");
        if (e.key === "4") setView("graph");
        if (e.key === "5") setView("workshop");
        if (e.key === "6") setView("metrics");
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [quickFileOpen, selectedBead, view]);

  const unread = MESSAGES.filter(m => !m.read).length;
  const activeAgents = AGENTS.filter(a => a.status === "active").length;

  const stats = {
    open: BEADS.filter(b => b.status === "open").length,
    in_progress: BEADS.filter(b => b.status === "in_progress").length,
    review: BEADS.filter(b => b.status === "review").length,
    closed: BEADS.filter(b => b.status === "closed").length,
  };

  const openBead = (b) => { if (b) setBead(b); };
  const openFormula = (formulaName) => {
    if (!formulaName) return;
    setWorkshopFormula(formulaName);
    setView("workshop");
    // intentionally leave selectedBead alone — the panel stays mounted behind
  };
  const toggleEpic = (id) => {
    setEpics(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  };

  return (
    <div style={{
      width: '100%', height: '100%',
      display: 'flex', flexDirection: 'column',
      background: 'var(--bg-1)', overflow: 'hidden',
    }}>
      <Header
        view={view}
        onSearch={setSearch}
        searchQuery={search}
        unreadCount={unread}
        activeAgents={activeAgents}
        connection={connection}
        onQuickFile={() => setQF(true)}
        theme={theme}
        onTheme={setTheme}
      />
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden', position: 'relative' }}>
        <NavRail view={view} onView={setView} unreadCount={unread}/>
        <main style={{ flex: 1, position: 'relative', overflow: 'hidden' }}>
          {view === "board"   && <BoardView onOpenBead={openBead} search={search} filter={filter} expandedEpics={expandedEpics} onToggleEpic={toggleEpic}/>}
          {view === "inbox"   && <InboxView messages={MESSAGES} onOpenBead={openBead} selectedId={selectedMsg} onSelect={setMsg}/>}
          {view === "roster"  && <RosterView onOpenBead={openBead}/>}
          {view === "graph"    && <GraphView onOpenBead={openBead}/>}
          {view === "workshop" && <WorkshopView onOpenBead={openBead} initialFormula={workshopFormula}/>}
          {view === "metrics"  && <MetricsView/>}
          {selectedBead && <DetailPanel bead={selectedBead} onClose={() => setBead(null)} onOpenBead={openBead} onOpenFormula={openFormula}/>}
        </main>
      </div>
      <StatusBar stats={stats}/>
      {quickFileOpen && <QuickFileModal onClose={() => setQF(false)}/>}

      {/* connection banner — appears when not connected */}
      {connection !== "connected" && (
        <div style={{
          position: 'absolute', bottom: 34, left: '50%', transform: 'translateX(-50%)',
          padding: '8px 14px',
          background: connection === "reconnecting" ? 'rgba(247,201,72,0.15)' : 'rgba(255,93,93,0.15)',
          border: `1px solid ${connection === "reconnecting" ? 'var(--sig-amber)' : 'var(--sig-red)'}`,
          borderRadius: 5,
          color: connection === "reconnecting" ? 'var(--sig-amber)' : 'var(--sig-red)',
          fontSize: 11, letterSpacing: 0.3,
          display: 'flex', alignItems: 'center', gap: 8,
        }} className="mono">
          <span className={connection === "reconnecting" ? "pulse-amber" : "pulse-red"}
                style={{ width: 7, height: 7, borderRadius: '50%', background: 'currentColor' }}/>
          {connection === "reconnecting"
            ? "reconnecting to dolt://spire-dolt.tower.svc:3306 · attempt 3/∞"
            : "cluster connection lost · local state may be stale"}
        </div>
      )}

      {/* Tweaks panel hook */}
      <TweaksBridge onConnectionChange={setConn} connection={connection}/>
    </div>
  );
}

// ── Tweaks ──────────────────────────────────────────────────
const TWEAK_DEFAULTS = /*EDITMODE-BEGIN*/{
  "connection": "connected",
  "density": "comfortable",
  "accent": "green",
  "showGrid": true,
  "showLiveTicker": true
}/*EDITMODE-END*/;

function TweaksBridge({ onConnectionChange, connection }) {
  const [tweaks, setTweaks] = useTweaks ? useTweaks(TWEAK_DEFAULTS) : [TWEAK_DEFAULTS, () => {}];

  React.useEffect(() => {
    if (tweaks.connection !== connection) onConnectionChange(tweaks.connection);
  }, [tweaks.connection]);

  React.useEffect(() => {
    const accentMap = {
      green: { pri: '#4fe38a', mint: '#2bd9a1' },
      cyan:  { pri: '#5ce1e6', mint: '#5ce1e6' },
      amber: { pri: '#f7c948', mint: '#f7c948' },
      violet:{ pri: '#b38cff', mint: '#b38cff' },
    };
    const a = accentMap[tweaks.accent] || accentMap.green;
    document.documentElement.style.setProperty('--sig-green', a.pri);
    document.documentElement.style.setProperty('--sig-mint', a.mint);
  }, [tweaks.accent]);

  if (!window.TweaksPanel) return null;

  return (
    <TweaksPanel title="Spire Tweaks">
      <TweakSection title="Connection">
        <TweakRadio label="Cluster state" value={tweaks.connection}
          onChange={v => setTweaks({ connection: v })}
          options={[
            { value: "connected",    label: "Connected" },
            { value: "reconnecting", label: "Reconnecting" },
            { value: "disconnected", label: "Disconnected" },
          ]}/>
      </TweakSection>
      <TweakSection title="Appearance">
        <TweakRadio label="Accent" value={tweaks.accent}
          onChange={v => setTweaks({ accent: v })}
          options={[
            { value: "green",  label: "Phosphor" },
            { value: "cyan",   label: "Cyan" },
            { value: "amber",  label: "Amber" },
            { value: "violet", label: "Violet" },
          ]}/>
        <TweakToggle label="Grid background" checked={tweaks.showGrid}
          onChange={v => setTweaks({ showGrid: v })}/>
        <TweakToggle label="Live ticker" checked={tweaks.showLiveTicker}
          onChange={v => setTweaks({ showLiveTicker: v })}/>
      </TweakSection>
    </TweaksPanel>
  );
}

// ── Mount ──────────────────────────────────────────────────
ReactDOM.createRoot(document.getElementById('root')).render(<App/>);
