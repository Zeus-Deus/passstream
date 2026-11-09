import { createRoot } from "react-dom/client";

// One sender and one viewer. Each surface loads its own code and theme.
const root = document.getElementById("root")!;
const path = location.pathname.replace(/\/+$/, "");
if (path === "/owner" && location.hostname === "127.0.0.1" && document.querySelector('meta[name="passstream-local"]')?.getAttribute("content") === "1") {
  void import("./sender/owner-portal").then((m) => m.mount(root));
} else if (path === "/sender") {
  // Served by `passstream host` on 127.0.0.1: the production sender.
  void import("./sender").then((m) => m.mountLocalSender(root));
} else if (path === "/host") {
  // Served by the server: the "trusted server" browser sender (HOST-CONTROL §2.1).
  void import("./sender").then((m) => m.mountBrowserSender(root));
} else if (path === "/v") {
  void import("./viewer").then((m) => m.mountViewer(root));
} else if (path === "/dashboard" || path.startsWith("/dashboard/")) {
  void import("./dashboard").then((m) => m.mount(root));
} else if (path === "/privacy") {
  void import("./viewer-ui").then((m) => m.mountPrivacy(root));
} else if (import.meta.env.DEV && path === "/dev/viewer-ui") {
  void import("./viewer-ui/Demo").then((m) => m.mount(root));
} else {
  void import("./home").then((m) => createRoot(root).render(<m.Home />));
}
