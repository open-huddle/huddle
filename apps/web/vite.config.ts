import { defineConfig } from "vite";
import react from "@vitejs/plugin-react-swc";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/huddle.v1": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
