import { expect, test } from "@playwright/test";
import { api, FakeSender, UA } from "./fake-sender";
import { env, login, noHorizontalOverflow, shot } from "./helpers";

const wide = (w: number) => w >= 640;

test("every dashboard screen renders against the real server", async ({ page }, info) => {
  const { base, showcase } = env();
  test.setTimeout(90_000);
  const width = page.viewportSize()!.width;

  await page.goto(`${base}/dashboard`);
  await expect(page.getByRole("heading", { name: "Sign in with a key." })).toBeVisible();
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-login");

  await login(page);
  const liveList = page.getByTestId("live-streams");
  await expect(liveList.getByText("Design review")).toBeVisible();
  await expect(page.getByTestId("ended-streams").getByText("Standup notes")).toBeVisible();
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-streams", true);

  await liveList.getByText("Design review").click();
  await expect(page).toHaveURL(`${base}/dashboard/streams/${showcase}`);
  await expect(page.getByText("Ana").first()).toBeVisible();
  // Rows come from the sender's admissions joined with server telemetry.
  await expect(page.getByTestId("row-code").filter({ hasText: "PFT" }).first()).toBeVisible();
  const leftRow = page.locator('[data-status="left"]').filter({ hasText: "RAW" });
  await expect(leftRow).toHaveCount(1);
  await expect(leftRow).toHaveClass(/opacity-55/);
  await expect(page.getByText("sender stale")).toHaveCount(0);
  // Knock queue is view-only.
  await expect(page.getByTestId("knocks").getByText("Approve on your computer (plugin/CLI)")).toBeVisible();
  await expect(page.getByTestId("knocks").getByRole("button", { name: /approve/i })).toHaveCount(0);
  // Batched failed-PIN alerts from the sender, relayed live over SSE (they
  // aren't replayed, so poke the sender while the page is subscribed).
  await expect(page.getByText(/Wrong PINs \d+\/20/)).toBeVisible();
  await expect(page.getByTestId("feed-status")).toHaveText("● live updates");
  await fetch(`${env().control}/failed-pin`);
  await expect(page.getByRole("alert")).toContainText("2 wrong PINs from 2+ sources");
  // Never a PIN value or a share link.
  await expect(page.getByText("PIN · 4 digits")).toBeVisible();
  await expect(page.getByText(/Link is on the host machine/)).toBeVisible();
  await expect(page.locator("body")).not.toContainText("/v#c=");
  await expect(page.getByTestId("feed-status")).toHaveText("● live updates");
  await expect(page.getByTestId("sender-health")).toContainText("online · browser");
  await expect(page.getByTestId(wide(width) ? "viewer-table" : "viewer-cards")).toBeVisible();
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-live", true);

  // Expanded row: IP, path and provenance.
  if (wide(width)) await page.getByRole("button", { name: "Show details for Ana" }).click();
  else await page.getByTestId("viewer-cards").getByText("Ana").click();
  const details = page.getByTestId("viewer-details");
  await expect(details).toContainText("203.0.113.20");
  await expect(details).toContainText("iPhone · Safari");
  await expect(details).toContainText("sender, via server");
  await expect(details).toContainText("viewer-reported");
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-live-expanded", true);

  await page.goto(`${base}/dashboard/history?stream=${showcase}`);
  await expect(page.getByTestId("audit")).toBeVisible();
  await expect(page.getByTestId("audit")).toContainText("Stream created");
  await expect(page.getByTestId("history")).toContainText("TEX");
  await expect(page.getByTestId("history")).toContainText("DENIED");
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-history", true);

  await page.goto(`${base}/dashboard/settings`);
  await expect(page.getByRole("heading", { name: "Defaults for new streams" })).toBeVisible();
  await expect(page.getByTestId("max-viewers")).toHaveValue("100");
  await noHorizontalOverflow(page);
  await shot(page, info, "dashboard-settings", true);
});

test("tightening actions work end to end; loosening is refused", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "one run of the state-changing flow");
  const { base, key } = env();
  const s = await FakeSender.create(base, key, { label: "Pairing session" });
  await s.connect();
  const ana = await s.viewer("Ana", UA.iphone, "203.0.113.30");
  const bo = await s.viewer("Bo", UA.mac, "203.0.113.31");
  const kim = await s.viewer("Kim", UA.android, "203.0.113.32");
  const aAna = s.admit(ana, "PFT");
  s.admit(bo, "K7P");
  s.knock(kim, "UPV");
  await s.settle();

  await login(page);
  await page.goto(`${base}/dashboard/streams/${s.stream.id}`);
  await expect(page.getByText("Ana")).toBeVisible();

  // Lock: the sender gets {locked:true}; Unlock is then local-only.
  await page.getByRole("button", { name: "Lock", exact: true }).click();
  await expect(page.getByText("Locked: no new viewers.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Unlock" })).toBeDisabled();
  await expect(page.getByText("LOCKED", { exact: true })).toBeVisible();

  // Kick from the expanded row; the row stays dimmed as "left".
  await page.getByRole("button", { name: "Show details for Ana" }).click();
  await page.getByRole("button", { name: "Kick", exact: true }).click();
  await expect(page.getByText("Kicked Ana.")).toBeVisible();
  await expect(page.locator('tr[data-status="left"]')).toContainText("Ana");

  // Deny the knock.
  await page.getByTestId("knocks").getByRole("button", { name: "Deny" }).click();
  await expect(page.getByText("Denied UPV.")).toBeVisible();
  await expect(page.getByText("No one is knocking.")).toBeVisible();

  // Ban Bo's IP: the server bans and sends the derived kick.
  await page.getByRole("button", { name: "Show details for Bo" }).click();
  await page.getByRole("button", { name: "Ban IP" }).click();
  await expect(page.getByText("Banned Bo's IP for this stream.")).toBeVisible();

  // Pause: Resume is local-only.
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(page.getByRole("button", { name: "Resume" })).toBeDisabled();
  await shot(page, info, "dashboard-live-after-actions", true);

  expect(s.controls.map((c) => c.op)).toEqual(["lock", "kick", "knock.deny", "kick", "pause"]);
  expect(s.controls[1].args).toEqual({ admission_id: aAna.admission_id });

  // The session can't loosen even by hand: the server answers 403.
  const r = await page.evaluate(async (id) => {
    const res = await fetch(`/api/v1/streams/${id}/commands`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ op: "resume", args: {} }) });
    return [res.status, (await res.json()).error.code];
  }, s.stream.id);
  expect(r).toEqual([403, "local_owner_required"]);

  // Audit shows what the dashboard did.
  await page.goto(`${base}/dashboard/history?stream=${s.stream.id}`);
  for (const op of ["Lock", "Kick", "Deny knock", "Ban IP", "Pause"]) await expect(page.getByTestId("audit").getByText(op, { exact: true }).first()).toBeVisible();

  // Stop ends it for everyone.
  await page.goto(`${base}/dashboard/streams/${s.stream.id}`);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await page.getByRole("button", { name: "Stop for everyone" }).click();
  await expect(page.getByText("Ended stream")).toBeVisible();
  const st = await api<{ ended_at: number | null }>(base, key, `/api/v1/streams/${s.stream.id}`);
  expect(st.ended_at).not.toBeNull();
  s.close();
});

test("settings, key management and sign-out", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "one run of the state-changing flow");
  const { base, key } = env();
  await login(page);
  await page.goto(`${base}/dashboard/settings`);
  await page.getByTestId("max-viewers").fill("42");
  await page.getByRole("button", { name: "Save defaults" }).click();
  await expect(page.getByText(/Saved\. New defaults apply/)).toBeVisible();
  await page.reload();
  await expect(page.getByTestId("max-viewers")).toHaveValue("42");

  // Keys need the owner key itself, not the session.
  await page.getByRole("textbox", { name: "Owner key" }).fill(key);
  await page.getByRole("button", { name: "Unlock key management" }).click();
  await expect(page.getByTestId("keys")).toBeVisible();
  await page.getByPlaceholder("e.g. laptop agent").fill("e2e agent");
  await page.getByRole("button", { name: "Create key" }).click();
  await expect(page.getByTestId("new-key")).toHaveText(/\S{20,}/);
  await expect(page.getByTestId("keys")).toContainText("e2e agent");
  await shot(page, info, "dashboard-settings-keys", true);
  await page.getByTestId("keys").locator("li", { hasText: "e2e agent" }).getByRole("button", { name: "Delete" }).click();
  await expect(page.getByTestId("keys")).not.toContainText("e2e agent");

  await page.getByTestId("max-viewers").fill("100");
  await page.getByRole("button", { name: "Save defaults" }).click();
  await expect(page.getByText(/Saved\./)).toBeVisible();

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { name: "Sign in with a key." })).toBeVisible();
  await page.goto(`${base}/dashboard`);
  await expect(page.getByRole("heading", { name: "Sign in with a key." })).toBeVisible();
});

test("the phase-1 pages still render with their own styles", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop", "one run");
  await page.goto(`${env().base}/`);
  await expect(page.getByRole("button", { name: "Create stream" })).toBeVisible();
  // style.css is applied (its header rule), and the dashboard theme isn't loaded.
  expect(await page.evaluate(() => getComputedStyle(document.querySelector("header")!).display)).toBe("flex");
  expect(await page.evaluate(() => [...document.styleSheets].some((s) => s.href?.includes("theme")))).toBe(false);
});
