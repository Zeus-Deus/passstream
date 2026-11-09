// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Privacy } from "./Privacy";

afterEach(cleanup);

describe("privacy notice describes E2EE accurately", () => {
  it("doesn't promise the server can't see content without conditions", () => {
    render(<Privacy />);
    const text = document.body.textContent!;
    expect(text).not.toMatch(/The server relays it but can't see it\./);
    expect(text).not.toMatch(/Nobody but you and the host/);
    expect(screen.getByRole("heading", { name: "Encryption, and its limits" })).toBeInTheDocument();
  });

  it("calls E2EE beta until an outside review", () => {
    render(<Privacy />);
    expect(screen.getByText(/End-to-end encryption is in beta\./)).toBeInTheDocument();
    expect(document.body).toHaveTextContent(/hasn't had an outside security review/);
  });

  it("says compromised page code defeats it", () => {
    render(<Privacy />);
    expect(document.body).toHaveTextContent(/only as trustworthy as the page code/);
    expect(document.body).toHaveTextContent(/can't protect you from compromised page code/);
  });

  it("describes the browser release's fail-closed behavior", () => {
    render(<Privacy />);
    expect(document.body).toHaveTextContent(/No unencrypted fallback/);
    expect(document.body).toHaveTextContent(/playback doesn't start; encryption isn't weakened/);
    expect(document.body).toHaveTextContent(/You, the host and other admitted viewers/);
    expect(document.body).not.toHaveTextContent(/legacy|WHIP|transport-only/);
  });

  it("lists what the server can see on encrypted streams", () => {
    render(<Privacy />);
    const p = screen.getByText(/What the server can always see/).closest("p")!;
    for (const s of ["IP address", "device and browser", "when you connect and leave", "size and timing", "layer metadata"]) expect(p).toHaveTextContent(s);
  });
});
