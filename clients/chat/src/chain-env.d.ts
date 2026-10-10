// The digest-chain module lives in the server's static tree (single
// source, byte-for-byte identical to the Go fold) — ambient types for
// the cross-tree import.
declare module "*/chain.mjs" {
  export const chainVersion: string;
  export function sha256Hex(text: string): Promise<string>;
  export function chainGenesis(sessionID: string): Promise<string>;
  export function chainStep(prev: string, r: unknown): Promise<string>;
}
