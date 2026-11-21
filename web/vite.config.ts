import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  // During development, API requests are proxied to a running TitoMail server
  // (set TITOMAIL_BACKEND to change it).
  const backend = loadEnv(mode, ".", "TITOMAIL_").TITOMAIL_BACKEND || "http://localhost:8080";
  return {
    plugins: [react()],
    build: {
      outDir: "dist",
      emptyOutDir: true,
      chunkSizeWarningLimit: 1500,
    },
    server: {
      proxy: {
        "/api": { target: backend },
        "/healthz": { target: backend },
        "/.well-known": { target: backend },
      },
    },
  };
});
