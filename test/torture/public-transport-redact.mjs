// Playwright errors can contain complete capability URLs, including fragments.
// Sanitize strings BEFORE JSON encoding so escaped quotes cannot break JSON.
export const redactText = value => String(value).replace(/((?:https?|wss?):\/\/[^\s"'<>#]+)#[^\s"'<>]+/g, "$1#[redacted]");
export function redactReport(value) {
  if (typeof value === "string") return redactText(value);
  if (value instanceof Error) return redactText(value.stack ?? value);
  if (Array.isArray(value)) return value.map(redactReport);
  if (value && typeof value === "object") return Object.fromEntries(Object.entries(value).map(([key, item]) => [redactText(key), redactReport(item)]));
  return value;
}
export const serializeReport = value => JSON.stringify(redactReport(value), null, 2) + "\n";
