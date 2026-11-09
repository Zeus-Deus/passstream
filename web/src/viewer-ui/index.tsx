import { StrictMode, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import "../assets/theme.css";
import { Privacy } from "./Privacy";

function render(el: HTMLElement, node: ReactNode) {
  createRoot(el).render(<StrictMode>{node}</StrictMode>);
}

export function mountPrivacy(el: HTMLElement) {
  document.title = "PassStream · Privacy notice";
  render(el, <Privacy />);
}
