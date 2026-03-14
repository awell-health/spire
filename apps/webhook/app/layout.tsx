export const metadata = {
  title: "Spire Webhook",
  description: "Linear webhook listener for Awell beads",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
