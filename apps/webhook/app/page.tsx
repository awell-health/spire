export default function Home() {
  return (
    <main style={{ padding: "2rem", fontFamily: "system-ui" }}>
      <h1>Spire Webhook</h1>
      <p>Linear webhook listener for Awell&apos;s beads tracking system.</p>
      <p>
        <code>POST /api/webhook</code> — receives Linear webhook events and
        queues them in DoltHub.
      </p>
    </main>
  );
}
