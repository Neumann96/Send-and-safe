export type AccessMode = "link" | "code";

export interface CryptoManifest {
  version: 1;
  accessMode: AccessMode;
  noncePrefix: string;
  metadataNonce: string;
  encryptedMetadata: string;
  keySalt?: string;
  keyNonce?: string;
  encryptedKey?: string;
  kdfIterations?: number;
}

export interface TransferManifest {
  id: string;
  plainSize: number;
  chunkSize: number;
  chunkCount: number;
  expiresAt: string;
  crypto: CryptoManifest;
}

export interface FileMetadata {
  name: string;
  type: string;
  size: number;
}
