import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "../assets/theme.css";
import { App } from "./App";

export function mount(el: HTMLElement) {
  document.title = "PassStream · Dashboard";
  createRoot(el).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
