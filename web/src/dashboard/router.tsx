import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from "react";

// The server answers every non-asset path with the SPA, so these are plain
// pushState routes under /dashboard.
export type Route =
  | { name: "streams" }
  | { name: "stream"; id: string }
  | { name: "history"; stream: string | null }
  | { name: "settings" };

export function parse(path: string, search = ""): Route {
  const p = path.replace(/\/+$/, "");
  const m = /^\/dashboard\/streams\/([A-Za-z0-9_-]{1,64})$/.exec(p);
  if (m) return { name: "stream", id: m[1] };
  if (p === "/dashboard/history")
    return { name: "history", stream: new URLSearchParams(search).get("stream") };
  if (p === "/dashboard/settings") return { name: "settings" };
  return { name: "streams" };
}

export function href(r: Route): string {
  switch (r.name) {
    case "stream":
      return `/dashboard/streams/${r.id}`;
    case "history":
      return r.stream ? `/dashboard/history?stream=${encodeURIComponent(r.stream)}` : "/dashboard/history";
    case "settings":
      return "/dashboard/settings";
    default:
      return "/dashboard";
  }
}

const listeners = new Set<() => void>();
if (typeof window !== "undefined")
  window.addEventListener("popstate", () => listeners.forEach((fn) => fn()));

export function navigate(to: string) {
  if (location.pathname + location.search === to) return;
  history.pushState(null, "", to);
  listeners.forEach((fn) => fn());
  window.scrollTo?.({ top: 0 });
}

export function useRoute(): Route {
  const key = useSyncExternalStore(
    (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    () => location.pathname + location.search,
  );
  const [path, search] = key.split("?");
  return parse(path, search ? `?${search}` : "");
}

export function Link({ to, onClick, ...rest }: { to: string } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a
      href={to}
      onClick={(e: MouseEvent<HTMLAnchorElement>) => {
        onClick?.(e);
        if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(to);
      }}
      {...rest}
    />
  );
}
