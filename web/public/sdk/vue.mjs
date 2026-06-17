// Stable alias for extension modules: they `import { h, ref, ... } from '/sdk/vue.mjs'`
// and get the shell's single Vue instance — no bundled copy, no version drift.
// See spec/frontend-extension-protocol.md §3.1.
export * from '/vendor/vue.esm-browser.prod.js'
