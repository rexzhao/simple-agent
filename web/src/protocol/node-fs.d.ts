declare module 'node:fs' {
  export interface Dirent {
    readonly name: string
    isDirectory(): boolean
  }
  export function readFileSync(path: URL | string, encoding: 'utf8'): string
  export function readdirSync(path: URL | string, options: { withFileTypes: true }): Dirent[]
}
