import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Base /chat/: the sai server mounts this build at /chat (Vite-absolute
// asset URLs must live under the mount point). The dev server proxies
// the API to a running `sai -http` on 7433 — the serve side answers
// CORS preflights for loopback origins.
export default defineConfig({
  base: "/chat/",
  plugins: [react()],
  server: {
    // The chain module is imported from the server's static tree.
    fs: { allow: ["../.."] },
    proxy: {
      "/api": "http://127.0.0.1:7433",
    },
  },
});
