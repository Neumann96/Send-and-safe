import { useEffect, useMemo, useState } from "react";
import {
  completeTransfer,
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
}

export default function App() {
  const transferID = useMemo(() => {
    const match = window.location.pathname.match(/^\/d\/([A-Za-z0-9_-]+)$/);
    return match?.[1] ?? null;
  }, []);

  useEffect(() => {
    const app = window.Telegram?.WebApp;
    app?.ready();
    app?.expand();
  }, []);

  return (
    <main className="shell">
      <header className="brand">
        <span className="brand-mark">S</span>
        <div>
          <strong>SendBigFiles</strong>
          <small>Приватная передача файлов</small>
        </div>
      </header>
      {transferID ? <DownloadView id={transferID} /> : <UploadView />}
      <footer>Файлы автоматически удаляются через 48 часов</footer>
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

  async function upload() {
    if (!file) return;
    if (file.size > MAX_FILE_BYTES) {
      setError("Для MVP максимальный размер файла — 512 МБ");
      return;
    }
    setBusy(true);
    setError("");
    setResult(null);
    try {
      const prepared = await prepareCrypto(file, mode);
      const chunkCount = Math.ceil(file.size / CHUNK_SIZE);
      const created = await createTransfer({
        plainSize: file.size,
        chunkSize: CHUNK_SIZE,
        chunkCount,
        crypto: prepared.manifest,
      });

      let nextIndex = 0;
      let completed = 0;
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
        }
      }
      await Promise.all([worker(), worker()]);
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
      setResult({ link, code: prepared.code, expiresAt: created.expiresAt });
    } catch (reason) {
      setError(messageOf(reason));
    } finally {
      setBusy(false);
    }
  }

  if (result) {
    return (
      <section className="card result-card">
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
        <button className="secondary" onClick={() => {
          setFile(null);
          setProgress(0);
          setResult(null);
        }}>
          Отправить другой файл
        </button>
      </section>
    );
  }

  return (
    <section className="card">
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
        <div className="progress-wrap">
          <div className="progress-line">
            <span style={{ width: `${progress}%` }} />
          </div>
          <small>Шифрование и загрузка: {progress}%</small>
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
    </section>
  );
}

function DownloadView({ id }: { id: string }) {
  const [manifest, setManifest] = useState<TransferManifest | null>(null);
  const [code, setCode] = useState("");
  const [progress, setProgress] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    getManifest(id).then(setManifest).catch((reason) => setError(messageOf(reason)));
  }, [id]);

  async function download() {
    if (!manifest) return;
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const secret = manifest.crypto.accessMode === "link" ? fragment.get("key") ?? "" : code;
    if (!secret) {
      setError("Нужен ключ из полной ссылки или код доступа");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const key = await resolveFileKey(manifest.crypto, secret);
      const metadata = await decryptMetadata(manifest.crypto, key);
      const parts: BlobPart[] = [];
      for (let index = 0; index < manifest.chunkCount; index += 1) {
        const encrypted = await getChunk(id, index);
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
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = metadata.name;
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch (reason) {
      setError(messageOf(reason, "Не удалось расшифровать файл"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="card download-card">
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
                placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"
                autoCapitalize="characters"
                autoCorrect="off"
              />
            </label>
          )}
          {busy && (
            <div className="progress-wrap">
              <div className="progress-line">
                <span style={{ width: `${progress}%` }} />
              </div>
              <small>Скачивание и расшифровка: {progress}%</small>
            </div>
          )}
          <button className="primary" disabled={busy} onClick={download}>
            {busy ? "Расшифровываем…" : "Скачать и расшифровать"}
          </button>
          <p className="notice">
            Расшифровка происходит только на этом устройстве.
          </p>
        </>
      )}
      {error && <p className="error">{error}</p>}
    </section>
  );
}

function ShareField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="share-field">
      <label>{label}</label>
      <div>
        <input value={value} readOnly />
        <button onClick={async () => {
          await navigator.clipboard.writeText(value);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        }}>
          {copied ? "Готово" : "Копировать"}
        </button>
      </div>
    </div>
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

function messageOf(reason: unknown, fallback = "Что-то пошло не так"): string {
  return reason instanceof Error ? reason.message : fallback;
}
