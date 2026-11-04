import { type FormEvent, useState } from "react";
import { Navigate } from "react-router-dom";
import { showTokenMint, canGrant, type Me } from "./access";
import { useAuth } from "./auth-context";
import { codeSpans, signOutEverywhereCopy } from "./signout";
import { Detail, DetailHeading, Field, FormError, H2, buttonClass, inputClass } from "./components/fb";
import { deleteJson, postJson, putJson, useJson } from "./useJson";

type Providers = { password?: boolean; github?: boolean };
type TokenRow = {
  id?: string;
  name?: string;
  hint?: string;
  created_at?: string;
  revoked_at?: string | null;
};
type GrantRow = { login?: string; login_kind?: string; level?: string; role?: string };

function parseMe(value: unknown): Me | null {
  if (typeof value !== "object" || value === null) return null;
  const body = value as { id?: unknown; role?: unknown; login?: unknown; login_kind?: unknown };
  if (typeof body.id !== "string" || typeof body.role !== "string" || typeof body.login !== "string") return null;
  return {
    id: body.id,
    role: body.role,
    login: body.login,
    login_kind: typeof body.login_kind === "string" ? body.login_kind : "",
  };
}

export function LoginPage() {
  const { state } = useAuth();
  if (state.status === "loading") return <div className="h-screen" />;
  if (state.status === "off" || state.status === "in") return <Navigate to="/" replace />;
  return <LoginForm />;
}

function LoginForm() {
  const { signedIn } = useAuth();
  const providers = useJson<Providers>("/api/auth/providers");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const github = providers.state === "ready" && providers.data.github === true;

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const name = username.trim();
    if (pending || !name || !password) return;
    setError("");
    setPending(true);
    const result = await postJson("/api/auth/login", { username: name, password });
    setPending(false);
    if (!result.ok) {
      setError(result.message || "unauthorized");
      return;
    }
    const account = parseMe(result.data);
    if (!account) {
      setError("unauthorized");
      return;
    }
    signedIn(account);
  }

  return (
    <div className="flex h-screen flex-col items-center justify-center gap-4 bg-background p-8">
      <h1 className="text-2xl font-semibold">Foreverbull</h1>
      <form
        className="w-full max-w-sm rounded-md border bg-card p-6 shadow-report"
        aria-label="Sign in"
        onSubmit={(event) => void onSubmit(event)}
      >
        <h2 className="mb-4 font-medium">Sign in</h2>
        <label className="mb-1 block text-muted-foreground" htmlFor="username">
          Username
        </label>
        <input
          id="username"
          className={inputClass}
          value={username}
          autoComplete="username"
          spellCheck={false}
          onChange={(event) => setUsername(event.target.value)}
        />
        <label className="mb-1 mt-3 block text-muted-foreground" htmlFor="password">
          Password
        </label>
        <input
          id="password"
          type="password"
          className={inputClass}
          value={password}
          autoComplete="current-password"
          onChange={(event) => setPassword(event.target.value)}
        />
        <button className={`${buttonClass} mt-4`} disabled={!username.trim() || !password || pending}>
          Sign in
        </button>
        {github ? (
          <a className="mt-4 block text-primary" href="/api/auth/github/start">
            GitHub
          </a>
        ) : null}
        <FormError message={error} />
      </form>
    </div>
  );
}

export function Locked({ account }: { account: Me }) {
  const line =
    account.role === "pending"
      ? "This account is not approved. It cannot open this installation."
      : "Access is revoked. This account cannot open this installation.";
  return (
    <div className="flex h-screen items-center justify-center">
      <div className="max-w-md">
        <p>{line}</p>
        <div className="mt-4">
          <LogoutButton />
        </div>
      </div>
    </div>
  );
}

export function LogoutButton() {
  const { signedOut } = useAuth();
  const [pending, setPending] = useState(false);

  async function onClick() {
    if (pending) return;
    setPending(true);
    await postJson("/api/auth/logout", {});
    signedOut();
  }

  return (
    <button type="button" className={buttonClass} disabled={pending} onClick={() => void onClick()}>
      Log out
    </button>
  );
}

// Revokes every token (CLI sign-ins and saved tokens) and every browser
// session of the account, this one included. It asks first.
function SignOutEverywhereButton() {
  const { signedOut } = useAuth();
  const [confirming, setConfirming] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const copy = signOutEverywhereCopy;

  async function onConfirm() {
    if (pending) return;
    setPending(true);
    setError("");
    const result = await postJson("/api/auth/revoke-all", {});
    setPending(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    signedOut();
  }

  if (!confirming) {
    return (
      <button type="button" className={buttonClass} onClick={() => setConfirming(true)}>
        {copy.button}
      </button>
    );
  }
  return (
    <div role="alertdialog" aria-labelledby="sign-out-everywhere-question" className="flex flex-col gap-2">
      <p id="sign-out-everywhere-question" className="font-medium">
        {copy.question}
      </p>
      <p className="text-muted-foreground">
        {codeSpans(copy.detail).map((piece) => (piece.code ? <code key={piece.text}>{piece.text}</code> : piece.text))}
      </p>
      <div className="flex gap-2">
        <button type="button" className={buttonClass} disabled={pending} onClick={() => void onConfirm()}>
          {copy.confirm}
        </button>
        <button type="button" className={buttonClass} disabled={pending} onClick={() => setConfirming(false)}>
          {copy.cancel}
        </button>
      </div>
      <FormError message={error} />
    </div>
  );
}

export function AccountPage() {
  const { state } = useAuth();
  if (state.status !== "in") {
    return (
      <Detail k="account">
        <p className="text-muted-foreground">Authentication is off.</p>
      </Detail>
    );
  }
  const account = state.account;
  return (
    <Detail k={account.id}>
      <DetailHeading title={account.login} subtitle="Account" />
      <dl>
        <Field label="Role">{account.role}</Field>
        <Field label="Login">{account.login}</Field>
      </dl>
      <div className="mt-6 flex gap-4">
        <LogoutButton />
        <SignOutEverywhereButton />
      </div>
      {showTokenMint(account) ? <TokenPanel /> : null}
    </Detail>
  );
}

function TokenPanel() {
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [secret, setSecret] = useState("");
  const [revision, setRevision] = useState(0);
  const list = useJson<TokenRow[]>("/api/auth/tokens", revision);
  const rows = list.state === "ready" && Array.isArray(list.data) ? list.data : [];

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = name.trim();
    if (pending || !trimmed) return;
    setError("");
    setPending(true);
    const result = await postJson("/api/auth/tokens", { name: trimmed });
    setPending(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    const minted = readToken(result.data);
    setName("");
    setSecret(minted);
    setRevision((current) => current + 1);
  }

  async function revoke(id: string) {
    const result = await deleteJson(`/api/auth/tokens/${encodeURIComponent(id)}`);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setRevision((current) => current + 1);
  }

  return (
    <section>
      <H2>Tokens</H2>
      <form className="max-w-md" onSubmit={(event) => void onSubmit(event)}>
        <label className="mb-1 block text-muted-foreground" htmlFor="token-name">
          Name
        </label>
        <input
          id="token-name"
          className={inputClass}
          value={name}
          spellCheck={false}
          autoComplete="off"
          onChange={(event) => setName(event.target.value)}
        />
        <button className={`${buttonClass} mt-4`} disabled={!name.trim() || pending}>
          Mint token
        </button>
        <FormError message={error} />
      </form>
      {secret ? (
        <p className="mt-4 break-all font-mono text-[13px]">
          {/* Shown once. Not written to localStorage or sessionStorage. */}
          {secret}
        </p>
      ) : null}
      <ul className="mt-4">
        {rows.map((row) => (
          <li key={row.id ?? row.hint} className="flex items-baseline gap-4 border-b py-1.5 font-mono text-[13px]">
            <span>{row.name}</span>
            <span className="text-muted-foreground">{row.hint}</span>
            {row.revoked_at ? <span className="text-muted-foreground">revoked</span> : null}
            {row.id && !row.revoked_at ? (
              <button type="button" className="text-primary" onClick={() => void revoke(row.id ?? "")}>
                Revoke
              </button>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

function readToken(value: unknown): string {
  if (typeof value !== "object" || value === null) return "";
  const token = (value as { token?: unknown }).token;
  return typeof token === "string" ? token : "";
}

function grantCollection(kind: "service" | "backtest" | "strategy", name: string): string {
  if (kind === "service") {
    return `/api/services/grants?${new URLSearchParams({ image: name }).toString()}`;
  }
  const root = kind === "backtest" ? "backtests" : "strategies";
  return `/api/${root}/${encodeURIComponent(name)}/grants`;
}

export function Grants({
  kind,
  name,
  ownerLogin,
}: {
  kind: "service" | "backtest" | "strategy";
  name: string;
  ownerLogin?: string;
}) {
  const { state } = useAuth();
  const me = state.status === "in" ? state.account : null;
  const allowed = canGrant(me, ownerLogin);
  const [login, setLogin] = useState("");
  const [level, setLevel] = useState<"viewer" | "editor">("viewer");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [revision, setRevision] = useState(0);
  const url = allowed && name ? grantCollection(kind, name) : null;
  const load = useJson<GrantRow[]>(url, revision);
  if (!allowed || !name) return null;
  const rows = load.state === "ready" && Array.isArray(load.data) ? load.data : [];

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = login.trim();
    if (pending || !trimmed || !url) return;
    setError("");
    setPending(true);
    const result = await putJson(url, { login: trimmed, level });
    setPending(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setLogin("");
    setRevision((current) => current + 1);
  }

  async function remove(grantee: string) {
    const target =
      kind === "service"
        ? `/api/services/grants?${new URLSearchParams({ image: name, login: grantee }).toString()}`
        : `${grantCollection(kind, name)}?${new URLSearchParams({ login: grantee }).toString()}`;
    const result = await deleteJson(target);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setRevision((current) => current + 1);
  }

  return (
    <section>
      <H2>Grants</H2>
      <ul>
        {rows.map((row) => (
          <li key={row.login} className="flex items-baseline gap-4 border-b py-1.5 font-mono text-[13px]">
            <span>{row.login}</span>
            <span>{row.level}</span>
            <span className="text-muted-foreground">{row.role}</span>
            {row.login ? (
              <button type="button" className="text-primary" onClick={() => void remove(row.login ?? "")}>
                Remove
              </button>
            ) : null}
          </li>
        ))}
      </ul>
      <form className="mt-4 max-w-md" onSubmit={(event) => void onSubmit(event)}>
        <label className="mb-1 block text-muted-foreground" htmlFor={`grant-login-${kind}`}>
          Login
        </label>
        <input
          id={`grant-login-${kind}`}
          className={inputClass}
          value={login}
          spellCheck={false}
          autoComplete="off"
          onChange={(event) => setLogin(event.target.value)}
        />
        <label className="mb-1 mt-3 block text-muted-foreground" htmlFor={`grant-level-${kind}`}>
          Level
        </label>
        <select
          id={`grant-level-${kind}`}
          className={inputClass}
          value={level}
          onChange={(event) => setLevel(event.target.value === "editor" ? "editor" : "viewer")}
        >
          <option value="viewer">viewer</option>
          <option value="editor">editor</option>
        </select>
        <button className={`${buttonClass} mt-4`} disabled={!login.trim() || pending}>
          Grant
        </button>
        <FormError message={error} />
      </form>
    </section>
  );
}
