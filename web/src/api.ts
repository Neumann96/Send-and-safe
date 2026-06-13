import type { CryptoManifest, TransferManifest } from "./types";

interface CreateRequest {
  plainSize: number;
  chunkSize: number;
  chunkCount: number;
  crypto: CryptoManifest;
}

interface CreatedTransfer {
  id: string;
  uploadToken: string;
  expiresAt: string;
}

async function checked(response: Response): Promise<Response> {
  if (response.ok) return response;
  const message = (await response.text()).trim();
  throw new Error(message || `Ошибка сервера: ${response.status}`);
}

export async function createTransfer(
  request: CreateRequest,
): Promise<CreatedTransfer> {
  const response = await checked(
    await fetch("/api/transfers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    }),
  );
  return response.json() as Promise<CreatedTransfer>;
}

export async function uploadChunk(
  id: string,
  token: string,
  index: number,
  data: ArrayBuffer,
): Promise<void> {
  await checked(
    await fetch(`/api/transfers/${id}/chunks/${index}`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${token}` },
      body: data,
    }),
  );
}

export async function completeTransfer(id: string, token: string): Promise<void> {
  await checked(
    await fetch(`/api/transfers/${id}/complete`, {
      method: "POST",
      headers: { Authorization: `Bearer ${token}` },
    }),
  );
}

export async function getManifest(id: string): Promise<TransferManifest> {
  const response = await checked(await fetch(`/api/transfers/${id}`));
  return response.json() as Promise<TransferManifest>;
}

export async function getChunk(id: string, index: number): Promise<ArrayBuffer> {
  const response = await checked(
    await fetch(`/api/transfers/${id}/chunks/${index}`),
  );
  return response.arrayBuffer();
}
