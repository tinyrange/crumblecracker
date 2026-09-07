import type { NextConfig } from "next";

const onGitHubPages = process.env.CRUMBLECRACKER_GITHUB_PAGES === "1";

const nextConfig: NextConfig = {
  output: "export",
  trailingSlash: true,
  basePath: onGitHubPages ? "/crumblecracker" : "",
  images: {
    unoptimized: true,
  },
};

export default nextConfig;
