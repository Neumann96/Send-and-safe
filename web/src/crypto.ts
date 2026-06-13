import type { AccessMode, CryptoManifest, FileMetadata } from "./types";

export const CHUNK_SIZE = 4 * 1024 * 1024;
export const KDF_ITERATIONS = 310_000;

const encoder = new TextEncoder();
const decoder = new TextDecoder();
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

export interface PreparedCrypto {
  fileKey: CryptoKey;
  exportedKey: string;
  code?: string;
  manifest: CryptoManifest;
}

export async function prepareCrypto(
  file: File,
  mode: AccessMode,
): Promise<PreparedCrypto> {
  const fileKey = await crypto.subtle.generateKey(
    { name: "AES-GCM", length: 256 },
    true,
    ["encrypt", "decrypt"],
  );
  const rawFileKey = new Uint8Array(await crypto.subtle.exportKey("raw", fileKey));
  const noncePrefix = crypto.getRandomValues(new Uint8Array(8));
  const metadataNonce = crypto.getRandomValues(new Uint8Array(12));
  const metadata: FileMetadata = {
    name: file.name,
    type: file.type || "application/octet-stream",
    size: file.size,
  };
  const encryptedMetadata = await crypto.subtle.encrypt(
    {
      name: "AES-GCM",
      iv: asBuffer(metadataNonce),
      additionalData: asBuffer(encoder.encode("sendbigfiles-metadata-v1")),
    },
    fileKey,
    asBuffer(encoder.encode(JSON.stringify(metadata))),
  );

  const manifest: CryptoManifest = {
    version: 1,
    accessMode: mode,
    noncePrefix: toBase64URL(noncePrefix),
    metadataNonce: toBase64URL(metadataNonce),
    encryptedMetadata: toBase64URL(new Uint8Array(encryptedMetadata)),
  };

  let code: string | undefined;
  if (mode === "code") {
    code = generateCode();
    const salt = crypto.getRandomValues(new Uint8Array(16));
    const keyNonce = crypto.getRandomValues(new Uint8Array(12));
    const wrappingKey = await deriveCodeKey(code, salt, KDF_ITERATIONS);
    const encryptedKey = await crypto.subtle.encrypt(
      {
        name: "AES-GCM",
        iv: asBuffer(keyNonce),
        additionalData: asBuffer(encoder.encode("sendbigfiles-key-v1")),
      },
      wrappingKey,
      asBuffer(rawFileKey),
    );
    manifest.keySalt = toBase64URL(salt);
    manifest.keyNonce = toBase64URL(keyNonce);
    manifest.encryptedKey = toBase64URL(new Uint8Array(encryptedKey));
    manifest.kdfIterations = KDF_ITERATIONS;
  }

  return {
    fileKey,
    exportedKey: toBase64URL(rawFileKey),
    code,
    manifest,
  };
}

export async function resolveFileKey(
  manifest: CryptoManifest,
  secret: string,
): Promise<CryptoKey> {
  if (manifest.accessMode === "link") {
    return importFileKey(fromBase64URL(secret));
  }
  if (
    !manifest.keySalt ||
    !manifest.keyNonce ||
    !manifest.encryptedKey ||
    !manifest.kdfIterations
  ) {
    throw new Error("Некорректный криптографический конверт");
  }
  const wrappingKey = await deriveCodeKey(
    normalizeCode(secret),
    fromBase64URL(manifest.keySalt),
    manifest.kdfIterations,
  );
  try {
    const rawKey = await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: asBuffer(fromBase64URL(manifest.keyNonce)),
        additionalData: asBuffer(encoder.encode("sendbigfiles-key-v1")),
      },
      wrappingKey,
      asBuffer(fromBase64URL(manifest.encryptedKey)),
    );
    return importFileKey(new Uint8Array(rawKey));
  } catch {
    throw new Error("Неверный код доступа");
  }
}

export async function decryptMetadata(
  manifest: CryptoManifest,
  fileKey: CryptoKey,
): Promise<FileMetadata> {
  const plain = await crypto.subtle.decrypt(
    {
      name: "AES-GCM",
      iv: asBuffer(fromBase64URL(manifest.metadataNonce)),
      additionalData: asBuffer(encoder.encode("sendbigfiles-metadata-v1")),
    },
    fileKey,
    asBuffer(fromBase64URL(manifest.encryptedMetadata)),
  );
  return JSON.parse(decoder.decode(plain)) as FileMetadata;
}

export async function encryptChunk(
  chunk: ArrayBuffer,
  fileKey: CryptoKey,
  noncePrefix: string,
  index: number,
  plainSize: number,
): Promise<ArrayBuffer> {
  return crypto.subtle.encrypt(
    {
      name: "AES-GCM",
      iv: asBuffer(chunkNonce(noncePrefix, index)),
      additionalData: asBuffer(chunkAAD(index, plainSize)),
    },
    fileKey,
    chunk,
  );
}

export async function decryptChunk(
  chunk: ArrayBuffer,
  fileKey: CryptoKey,
  noncePrefix: string,
  index: number,
  plainSize: number,
): Promise<ArrayBuffer> {
  return crypto.subtle.decrypt(
    {
      name: "AES-GCM",
      iv: asBuffer(chunkNonce(noncePrefix, index)),
      additionalData: asBuffer(chunkAAD(index, plainSize)),
    },
    fileKey,
    chunk,
  );
}

function chunkNonce(prefix: string, index: number): Uint8Array {
  const nonce = new Uint8Array(12);
  nonce.set(fromBase64URL(prefix), 0);
  new DataView(nonce.buffer).setUint32(8, index, false);
  return nonce;
}

function chunkAAD(index: number, plainSize: number): Uint8Array {
  return encoder.encode(`sendbigfiles-chunk-v1:${index}:${plainSize}`);
}

async function deriveCodeKey(
  code: string,
  salt: Uint8Array,
  iterations: number,
): Promise<CryptoKey> {
  const material = await crypto.subtle.importKey(
    "raw",
    asBuffer(encoder.encode(normalizeCode(code))),
    "PBKDF2",
    false,
    ["deriveKey"],
  );
  return crypto.subtle.deriveKey(
    { name: "PBKDF2", hash: "SHA-256", salt: asBuffer(salt), iterations },
    material,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

function importFileKey(raw: Uint8Array): Promise<CryptoKey> {
  if (raw.byteLength !== 32) {
    throw new Error("Некорректный ключ в ссылке");
  }
  return crypto.subtle.importKey("raw", asBuffer(raw), "AES-GCM", false, [
    "encrypt",
    "decrypt",
  ]);
}

function generateCode(): string {
  const random = crypto.getRandomValues(new Uint8Array(24));
  const chars = Array.from(random, (value) => codeAlphabet[value % 32]);
  return Array.from({ length: 6 }, (_, index) =>
    chars.slice(index * 4, index * 4 + 4).join(""),
  ).join("-");
}

export function normalizeCode(code: string): string {
  return code.toUpperCase().replace(/[^A-Z2-9]/g, "");
}

export function toBase64URL(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

export function fromBase64URL(value: string): Uint8Array {
  const padded = value.replaceAll("-", "+").replaceAll("_", "/")
    .padEnd(Math.ceil(value.length / 4) * 4, "=");
  const binary = atob(padded);
  return Uint8Array.from(binary, (char) => char.charCodeAt(0));
}

function asBuffer(bytes: Uint8Array): ArrayBuffer {
  return bytes.slice().buffer as ArrayBuffer;
}
