import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import LiquidGlass from "liquid-glass-react";
import {
  ApiError,
  claimTransfer,
  completeTransfer,
  consumeTransfer,
  createTransfer,
  getChunk,
  getManifest,
  uploadChunk,
} from "./api";
import {
  CHUNK_SIZE,
  decryptChunk,
  decryptMetadata,
  encryptChunk,
  prepareCrypto,
  resolveFileKey,
} from "./crypto";
import type { AccessMode, TransferManifest } from "./types";

const MAX_FILE_BYTES = 512 * 1024 * 1024;

interface ShareResult {
  link: string;
  code?: string;
  expiresAt: string;
  chunkCount: number;
  durationMs: number;
}

function GlassCard({
  className = "",
  children,
}: {
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className="card-glass-wrap">
      <LiquidGlass
        className="card-glass-effect"
        displacementScale={76}
        blurAmount={0.01}
        saturation={150}
        aberrationIntensity={2.4}
        elasticity={0}
        cornerRadius={25}
        padding="0"
        mode="prominent"
        style={{ width: "100%" }}
      >
        <section className={`card ${className}`.trim()}>{children}</section>
      </LiquidGlass>
    </div>
  );
}

export default function App() {
  const route = useMemo(() => {
    if (window.location.pathname === "/") {
      return { kind: "upload" } as const;
    }
    const match = window.location.pathname.match(/^\/d\/([A-Za-z0-9_-]+)$/);
    if (match) {
      return { kind: "download", id: match[1] } as const;
    }
    return { kind: "not-found" } as const;
  }, []);

  useEffect(() => {
    const app = window.Telegram?.WebApp;
    if (!app) return;

    const root = document.documentElement;
    const syncTelegramViewport = () => {
      const inset = app.contentSafeAreaInset;
      root.classList.toggle("telegram-fullscreen", app.isFullscreen);
      root.classList.toggle(
        "telegram-fullscreen-mobile",
        app.isFullscreen && (app.platform === "ios" || app.platform === "android"),
      );
      root.style.setProperty("--app-content-safe-area-top", `${inset?.top ?? 0}px`);
      root.style.setProperty("--app-content-safe-area-right", `${inset?.right ?? 0}px`);
      root.style.setProperty("--app-content-safe-area-bottom", `${inset?.bottom ?? 0}px`);
      root.style.setProperty("--app-content-safe-area-left", `${inset?.left ?? 0}px`);
    };

    const backgroundColor = "#080b12";
    app.setHeaderColor(backgroundColor);
    app.setBackgroundColor(backgroundColor);
    if (app.isVersionAtLeast("7.10")) {
      app.setBottomBarColor(backgroundColor);
    }

    app.ready();
    app.expand();
    if (app.isVersionAtLeast("8.0")) {
      app.onEvent("fullscreenChanged", syncTelegramViewport);
      app.onEvent("contentSafeAreaChanged", syncTelegramViewport);
      syncTelegramViewport();
    }

    return () => {
      if (app.isVersionAtLeast("8.0")) {
        app.offEvent("fullscreenChanged", syncTelegramViewport);
        app.offEvent("contentSafeAreaChanged", syncTelegramViewport);
      }
      root.classList.remove("telegram-fullscreen", "telegram-fullscreen-mobile");
      root.style.removeProperty("--app-content-safe-area-top");
      root.style.removeProperty("--app-content-safe-area-right");
      root.style.removeProperty("--app-content-safe-area-bottom");
      root.style.removeProperty("--app-content-safe-area-left");
    };
  }, []);

  return (
    <main className="shell">
      <header className="brand">
        <span className="brand-file" aria-hidden="true">
          <i className="brand-file-sheet">
            <b />
            <b />
          </i>
          <i className="brand-file-lock" />
        </span>
        <div>
          <strong>Send and Safe</strong>
          <small>Приватная передача файлов</small>
        </div>
      </header>
      {route.kind === "upload" && <UploadView />}
      {route.kind === "download" && <DownloadView id={route.id} />}
      {route.kind === "not-found" && <NotFoundView />}
      {route.kind !== "not-found" && (
        <footer>Файлы автоматически удаляются через 48 часов</footer>
      )}
    </main>
  );
}

function UploadView() {
  const [file, setFile] = useState<File | null>(null);
  const [mode, setMode] = useState<AccessMode>("link");
  const [progress, setProgress] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<ShareResult | null>(null);
  const [stage, setStage] = useState("Ожидание файла");

  async function upload() {
    if (!file) return;
    if (file.size > MAX_FILE_BYTES) {
      setError("Для MVP максимальный размер файла — 512 МБ");
      return;
    }
    setBusy(true);
    setError("");
    setResult(null);
    setProgress(0);
    const startedAt = performance.now();
    try {
      setStage("Создаём локальный ключ AES-256");
      const prepared = await prepareCrypto(file, mode);
      const chunkCount = Math.ceil(file.size / CHUNK_SIZE);
      setStage("Передаём серверу только зашифрованный конверт");
      const created = await createTransfer({
        plainSize: file.size,
        chunkSize: CHUNK_SIZE,
        chunkCount,
        crypto: prepared.manifest,
      });

      let nextIndex = 0;
      let completed = 0;
      setStage(`Шифруем и отправляем блоки: 0 из ${chunkCount}`);
      async function worker() {
        while (nextIndex < chunkCount) {
          const index = nextIndex++;
          const start = index * CHUNK_SIZE;
          const plain = await file!.slice(start, start + CHUNK_SIZE).arrayBuffer();
          const encrypted = await encryptChunk(
            plain,
            prepared.fileKey,
            prepared.manifest.noncePrefix,
            index,
            file!.size,
          );
          await uploadChunk(created.id, created.uploadToken, index, encrypted);
          completed += 1;
          setProgress(Math.round((completed / chunkCount) * 100));
          setStage(`Зашифровано и отправлено: ${completed} из ${chunkCount}`);
        }
      }
      await Promise.all([worker(), worker()]);
      setStage("Сервер проверяет наличие всех зашифрованных блоков");
      await completeTransfer(created.id, created.uploadToken);

      const base = `${window.location.origin}/d/${created.id}`;
      const link =
        mode === "link" ? `${base}#key=${prepared.exportedKey}` : base;
      localStorage.setItem(
        `sendbigfiles:${created.id}`,
        JSON.stringify({
          token: created.uploadToken,
          expiresAt: created.expiresAt,
        }),
      );
      setResult({
        link,
        code: prepared.code,
        expiresAt: created.expiresAt,
        chunkCount,
        durationMs: performance.now() - startedAt,
      });
      hapticSuccess();
    } catch (reason) {
      setError(messageOf(reason));
      hapticError();
    } finally {
      setBusy(false);
    }
  }

  if (result) {
    return (
      <GlassCard className="result-card">
        <div className="success">Готово</div>
        <h1>Файл зашифрован и загружен</h1>
        <p className="muted">Доступ автоматически закроется через 48 часов.</p>
        <ShareField label="Ссылка" value={result.link} />
        {result.code && (
          <>
            <ShareField label="Код доступа" value={result.code} />
            <p className="notice">
              Отправьте код отдельно от ссылки. Сервер не знает этот код.
            </p>
          </>
        )}
        <div className="proof-card">
          <div className="proof-title">
            <span className="proof-icon">✓</span>
            <div>
              <strong>Что произошло на этом устройстве</strong>
              <small>{result.chunkCount} зашифрованных блоков за {formatDuration(result.durationMs)}</small>
            </div>
          </div>
          <div className="proof-list">
            <span>Создан случайный ключ AES-256</span>
            <span>Имя и тип файла зашифрованы</span>
            <span>Сервер получил только шифротекст</span>
          </div>
        </div>
        <TransparencyDetails mode={mode} />
        <button className="secondary" onClick={() => {
          setFile(null);
          setProgress(0);
          setResult(null);
          setStage("Ожидание файла");
        }}>
          Отправить другой файл
        </button>
      </GlassCard>
    );
  }

  return (
    <GlassCard>
      <div className="eyebrow">END-TO-END ШИФРОВАНИЕ</div>
      <h1>Передайте файл без лишних следов</h1>
      <p className="lead">
        Файл шифруется на вашем устройстве. Мы храним только зашифрованные блоки.
      </p>

      <label className="dropzone">
        <input
          type="file"
          disabled={busy}
          onChange={(event) => {
            setFile(event.target.files?.[0] ?? null);
            setError("");
          }}
        />
        <span className="upload-icon">↑</span>
        {file ? (
          <>
            <strong>{file.name}</strong>
            <small>{formatBytes(file.size)}</small>
          </>
        ) : (
          <>
            <strong>Выберите файл</strong>
            <small>До 512 МБ</small>
          </>
        )}
      </label>

      <div className="mode-label">Как предоставить доступ</div>
      <div className="mode-grid">
        <button
          className={mode === "link" ? "mode active" : "mode"}
          onClick={() => setMode("link")}
          disabled={busy}
        >
          <strong>Секретная ссылка</strong>
          <span>Одна ссылка, самый быстрый способ</span>
        </button>
        <button
          className={mode === "code" ? "mode active" : "mode"}
          onClick={() => setMode("code")}
          disabled={busy}
        >
          <strong>Ссылка + код</strong>
          <span>Код можно передать другим каналом</span>
        </button>
      </div>

      {busy && (
        <div className="progress-wrap" aria-live="polite">
          <div className="progress-line">
            <span style={{ width: `${progress}%` }} />
          </div>
          <small>{stage} · {progress}%</small>
        </div>
      )}
      {error && <p className="error">{error}</p>}
      <button className="primary" disabled={!file || busy} onClick={upload}>
        {busy ? "Защищаем и загружаем…" : "Зашифровать и загрузить"}
      </button>
      <div className="privacy-row">
        <span>Без регистрации</span>
        <span>Без передачи ключей серверу</span>
      </div>
      <TransparencyDetails mode={mode} compact />
    </GlassCard>
  );
}

function DownloadView({ id }: { id: string }) {
  const [manifest, setManifest] = useState<TransferManifest | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [code, setCode] = useState("");
  const [progress, setProgress] = useState(0);
  const [busy, setBusy] = useState(false);
  const [completed, setCompleted] = useState(false);
  const [error, setError] = useState("");
  const [stage, setStage] = useState("Готово к скачиванию");

  useEffect(() => {
    getManifest(id)
      .then(setManifest)
      .catch((reason) => {
        if (isUnavailable(reason)) {
          setUnavailable(true);
          return;
        }
        setError(messageOf(reason));
      });
  }, [id]);

  async function download() {
    if (!manifest || busy || completed) return;
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const secret = manifest.crypto.accessMode === "link" ? fragment.get("key") ?? "" : code;
    if (!secret) {
      setError("Нужен ключ из полной ссылки или код доступа");
      return;
    }
    setBusy(true);
    setError("");
    setProgress(0);
    try {
      setStage("Восстанавливаем ключ только на этом устройстве");
      const key = await resolveFileKey(manifest.crypto, secret);
      setStage("Расшифровываем имя и тип файла");
      const metadata = await decryptMetadata(manifest.crypto, key);
      setStage("Резервируем единственное получение файла");
      const downloadToken = await claimTransfer(id);
      const parts: BlobPart[] = [];
      for (let index = 0; index < manifest.chunkCount; index += 1) {
        setStage(`Скачиваем и расшифровываем блок ${index + 1} из ${manifest.chunkCount}`);
        const encrypted = await getChunk(id, downloadToken, index);
        const plain = await decryptChunk(
          encrypted,
          key,
          manifest.crypto.noncePrefix,
          index,
          manifest.plainSize,
        );
        parts.push(plain);
        setProgress(Math.round(((index + 1) / manifest.chunkCount) * 100));
      }
      const url = URL.createObjectURL(new Blob(parts, { type: metadata.type }));
      setStage("Удаляем зашифрованную копию с сервера");
      await consumeTransfer(id, downloadToken);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = metadata.name;
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
      setCompleted(true);
      setStage("Файл расшифрован и передан браузеру");
      hapticSuccess();
    } catch (reason) {
      if (isUnavailable(reason)) {
        setUnavailable(true);
        return;
      }
      setError(messageOf(reason, "Не удалось расшифровать файл"));
      hapticError();
    } finally {
      setBusy(false);
    }
  }

  if (unavailable) {
    return <NotFoundView />;
  }

  return (
    <GlassCard className="download-card">
      <div className="eyebrow">ЗАЩИЩЁННЫЙ ФАЙЛ</div>
      <h1>Получить файл</h1>
      {!manifest && !error && <p className="muted">Проверяем ссылку…</p>}
      {manifest && (
        <>
          <div className="file-summary">
            <span className="file-lock">◇</span>
            <div>
              <strong>Зашифрованный файл</strong>
              <small>{formatBytes(manifest.plainSize)} · доступен до {formatDate(manifest.expiresAt)}</small>
            </div>
          </div>
          {manifest.crypto.accessMode === "code" && (
            <label className="code-input">
              <span>Код доступа</span>
              <input
                value={code}
                onChange={(event) => setCode(event.target.value.toUpperCase())}
                disabled={busy || completed}
                placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"
                autoCapitalize="characters"
                autoCorrect="off"
              />
            </label>
          )}
          {busy && (
            <div className="progress-wrap" aria-live="polite">
              <div className="progress-line">
                <span style={{ width: `${progress}%` }} />
              </div>
              <small>{stage} · {progress}%</small>
            </div>
          )}
          <button
            className={completed ? "primary completed" : "primary"}
            disabled={busy || completed}
            onClick={download}
          >
            {completed
              ? "Файл успешно расшифрован"
              : busy
                ? "Расшифровываем…"
                : "Скачать и расшифровать"}
          </button>
          <p className="notice">
            Файл можно получить один раз. После расшифровки сервер удаляет его зашифрованную копию.
          </p>
        </>
      )}
      {error && <p className="error">{error}</p>}
    </GlassCard>
  );
}

function NotFoundView() {
  useEffect(() => {
    const previousTitle = document.title;
    document.title = "След файла потерян — Send and Safe";
    return () => {
      document.title = previousTitle;
    };
  }, []);

  return (
    <GlassCard className="lost-card">
      <div className="lost-scene" aria-hidden="true">
        <div className="lost-orbit lost-orbit-one" />
        <div className="lost-orbit lost-orbit-two" />
        <div className="lost-code">404</div>
        <div className="lost-file">
          <span />
          <span />
          <span />
        </div>
        <i className="lost-pixel pixel-one" />
        <i className="lost-pixel pixel-two" />
        <i className="lost-pixel pixel-three" />
        <i className="lost-pixel pixel-four" />
      </div>
      <div className="lost-copy">
        <div className="eyebrow">ЦИФРОВОЙ СЛЕД ОБОРВАЛСЯ</div>
        <h1>Здесь был файл.<br />Теперь только тишина.</h1>
        <p>
          Возможно, его уже расшифровали, срок хранения закончился или ссылка
          ведёт в несуществующее место. Мы не оставляем копий, поэтому вернуть
          его отсюда нельзя.
        </p>
        <div className="lost-status">
          <span className="lost-status-dot" />
          <span>Зашифрованных данных на сервере не найдено</span>
        </div>
        <button className="primary lost-action" onClick={() => window.location.assign("/")}>
          Отправить новый файл
          <span aria-hidden="true">→</span>
        </button>
        <small className="lost-footnote">И это, пожалуй, хорошая новость для приватности.</small>
      </div>
    </GlassCard>
  );
}

function ShareField({ label, value }: { label: string; value: string }) {
  const [copyState, setCopyState] = useState<"idle" | "copied" | "error">("idle");

  async function copy() {
    try {
      await copyText(value);
      setCopyState("copied");
      window.Telegram?.WebApp.HapticFeedback?.impactOccurred("light");
      setTimeout(() => setCopyState("idle"), 1600);
    } catch {
      setCopyState("error");
    }
  }

  return (
    <div className="share-field">
      <label>{label}</label>
      <div>
        <input
          value={value}
          readOnly
          onFocus={(event) => event.currentTarget.select()}
          aria-label={label}
        />
        <button className={copyState === "copied" ? "copied" : ""} onClick={copy}>
          {copyState === "copied"
            ? "Скопировано"
            : copyState === "error"
              ? "Выделить"
              : "Копировать"}
        </button>
      </div>
      {copyState === "error" && (
        <small className="copy-help">Нажмите на поле и скопируйте выделенный текст.</small>
      )}
    </div>
  );
}

function TransparencyDetails({
  mode,
  compact = false,
}: {
  mode: AccessMode;
  compact?: boolean;
}) {
  return (
    <details className={compact ? "transparency compact" : "transparency"}>
      <summary>Как проверить приватность</summary>
      <div className="transparency-content">
        <p><strong>Сервер получает:</strong> размер, время, IP-адрес и зашифрованные блоки.</p>
        <p><strong>Сервер не получает:</strong> имя файла, содержимое и {
          mode === "code" ? "код доступа" : "ключ из части ссылки после #"
        }.</p>
        <p>Имя файла сохраняется, потому что оно находится внутри зашифрованных метаданных и восстанавливается у получателя.</p>
      </div>
    </details>
  );
}

function formatBytes(bytes: number): string {
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} КБ`;
  return `${(bytes / 1024 / 1024).toFixed(1)} МБ`;
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat("ru", {
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(value));
}

function formatDuration(milliseconds: number): string {
  if (milliseconds < 1000) return `${Math.round(milliseconds)} мс`;
  return `${(milliseconds / 1000).toFixed(1)} с`;
}

async function copyText(value: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      // Telegram WebView can expose Clipboard API while denying the write.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.readOnly = true;
  textarea.style.position = "fixed";
  textarea.style.inset = "0";
  textarea.style.width = "1px";
  textarea.style.height = "1px";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, value.length);
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("Clipboard is unavailable");
}

function hapticSuccess() {
  window.Telegram?.WebApp.HapticFeedback?.notificationOccurred("success");
}

function hapticError() {
  window.Telegram?.WebApp.HapticFeedback?.notificationOccurred("error");
}

function messageOf(reason: unknown, fallback = "Что-то пошло не так"): string {
  return reason instanceof Error ? reason.message : fallback;
}

function isUnavailable(reason: unknown): boolean {
  return reason instanceof ApiError && (reason.status === 404 || reason.status === 410);
}
