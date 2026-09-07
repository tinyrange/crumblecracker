import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  metadataBase: new URL("https://tinyrange.github.io/crumblecracker/"),
  title: "CrumbleCracker — Desktop virtual machines",
  description:
    "Download NeurodeskAppX and SquadVM for macOS, Windows, and Linux.",
  icons: {
    icon: "./favicon.svg",
    shortcut: "./favicon.svg",
  },
  openGraph: {
    type: "website",
    url: "https://tinyrange.github.io/crumblecracker/",
    title: "CrumbleCracker — Your lab, ready to run.",
    description:
      "Download NeurodeskAppX and SquadVM for macOS, Windows, and Linux.",
  },
  twitter: {
    card: "summary",
    title: "CrumbleCracker — Your lab, ready to run.",
    description:
      "Download NeurodeskAppX and SquadVM for macOS, Windows, and Linux.",
  },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body
        className={`${geistSans.variable} ${geistMono.variable} antialiased`}
      >
        {children}
      </body>
    </html>
  );
}
