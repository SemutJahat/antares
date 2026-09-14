// Prints the initial-download totals and top module contributors for each
// entry in web/dist/bundle-report.json, produced by the Vite plugin in
// web/vite.config.ts. Read after `bun run --cwd web build` — no rebuild here,
// so the same artifact serves CI, local diffing, and hand inspection.
//
//   bun scripts/bundle-report.mjs               # top 20 modules per entry
//   bun scripts/bundle-report.mjs 40            # top 40 modules per entry
//   REPORT=path/to/bundle-report.json bun scripts/bundle-report.mjs
import { readFileSync, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const __dirname = dirname(fileURLToPath(import.meta.url))
const reportPath = process.env.REPORT
  ?? resolve(__dirname, '..', 'web', 'dist', 'bundle-report.json')
const topN = Number(process.argv[2] ?? 20)

if (!existsSync(reportPath)) {
  console.error(`bundle report not found at ${reportPath}`)
  console.error('run `bun run --cwd web build` first (vite plugin writes it).')
  process.exit(2)
}

const report = JSON.parse(readFileSync(reportPath, 'utf8'))
const kb = (n) => (n / 1024).toFixed(1).padStart(7) + ' KB'
const relative = (id) => id.replace(process.cwd() + '/', '').replace(/^.*\/node_modules\//, 'node_modules/')

console.log(`bundle report  ${report.generatedAt}`)
console.log(`source         ${reportPath}\n`)

for (const entry of report.entries) {
  console.log(`entry ${entry.entry}  (${entry.name})`)
  console.log(`  initial closure  ${entry.chunkCount} chunks`)
  console.log(`  raw              ${kb(entry.rawBytes)}`)
  console.log(`  gzip             ${kb(entry.gzipBytes)}`)

  console.log(`  chunks:`)
  for (const c of entry.chunks.slice(0, Math.min(10, entry.chunks.length))) {
    console.log(`    ${kb(c.rawBytes)} raw / ${kb(c.gzipBytes)} gz  ${c.file}`)
  }
  if (entry.chunks.length > 10) {
    console.log(`    … ${entry.chunks.length - 10} more chunk(s)`)
  }

  console.log(`  top ${Math.min(topN, entry.modules.length)} modules by rendered bytes:`)
  for (const m of entry.modules.slice(0, topN)) {
    console.log(`    ${kb(m.renderedLength)}  ${relative(m.id)}   [${m.chunk}]`)
  }
  console.log()
}
