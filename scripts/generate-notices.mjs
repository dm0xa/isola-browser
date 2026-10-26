#!/usr/bin/env node
// Generates the repository NOTICE file from the dependency trees that are
// actually resolved, so the module list, versions, licenses and
// copyright lines are read from disk instead of being written by hand.
//
//   node scripts/generate-notices.mjs          # writes ./NOTICE
//   node scripts/generate-notices.mjs --check  # exits 1 if NOTICE is stale
//
// Sources: backend/go.mod (+ the Go module cache),
// browsers/isola-streamer/go.mod (+ the Go module cache),
// browsers/electron-runner/package.json, frontend/package-lock.json
// (+ frontend/node_modules).
//
// Nothing here may read backend/vendor: vendor/ is gitignored and absent in CI
// and in the release build (both use `go mod download`), so a vendor-based list
// made this file's contents depend on the machine that generated it — a stale
// vendor tree silently published old versions.
//
// Trees that are not part of this repository are deliberately excluded from
// the generated NOTICE.

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const OUT = path.join(ROOT, 'NOTICE');

const read = (p) => (fs.existsSync(p) ? fs.readFileSync(p, 'utf8') : null);
const LICENSE_RE = /^(LICEN[CS]E|COPYING)(\.(md|txt|rst))?$/i;

// Placeholders used when a pinned package ships no license file of its own
// (its terms are then taken from the SPDX id in package.json / go.mod).
const NO_LICENSE_FILE = '_(no license file in package — see SPDX id)_';
const NO_COPYRIGHT = '_(not stated in license file)_';

// ---------------------------------------------------------------- helpers

function licenseFiles(dir) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  return entries
    .filter((e) => e.isFile() && LICENSE_RE.test(e.name))
    .map((e) => path.join(dir, e.name));
}

/** Every license marker found in a license file (a file may be dual-licensed). */
function detectLicenses(text) {
  if (!text) return [];
  const found = [];
  const add = (id) => {
    if (!found.includes(id)) found.push(id);
  };
  if (/Apache License[,\s]+(Version )?2\.0/i.test(text)) add('Apache-2.0');
  if (/Mozilla Public License/i.test(text)) add('MPL-2.0');
  if (/GNU (LESSER|GENERAL) PUBLIC LICENSE/i.test(text)) add('GPL/LGPL');
  if (/Permission is hereby granted, free of charge/i.test(text)) add('MIT');
  if (/Permission to use, copy, modify, and(\/or)? distribute this software/i.test(text)) add('ISC');
  if (/Neither the name/i.test(text)) add('BSD-3-Clause');
  else if (/Redistribution and use in source and binary forms/i.test(text)) add('BSD-2-Clause');
  if (/Boost Software License/i.test(text)) add('BSL-1.0');
  if (/The Unlicense/i.test(text)) add('Unlicense');
  if (/\bCC0\b/.test(text)) add('CC0-1.0');
  return found;
}

/** Copyright-holder lines in a license file, deduped and joined for a table cell. */
function copyrightLines(text) {
  if (!text) return null;
  const lines = [];
  const keys = new Set();
  for (const m of text.matchAll(/^[ \t]*(Copyright\b[^\n]*)$/gim)) {
    const line = m[1].trim();
    // Must be an actual holder line ("Copyright (c) ..." / "Copyright 2024 ..."),
    // not the boilerplate or prose that mentions the word in passing.
    if (!/^Copyright\s*(\(c\)|©|\d{4})/i.test(line)) continue;
    if (/copyright (owner|holder|notice that)/i.test(line)) continue;
    const key = line.replace(/\d{4}(-\d{4})?/g, '').replace(/\s+/g, ' ').toLowerCase();
    if (keys.has(key)) continue;
    keys.add(key);
    lines.push(line);
  }
  return lines.length ? lines.join('; ') : null;
}

/** Pick the best license text + copyright for a directory. */
function inspectDir(dir) {
  for (const f of licenseFiles(dir)) {
    const text = read(f);
    if (!text) continue;
    const licenses = detectLicenses(text);
    if (licenses.length) {
      return { license: licenses.join(' AND '), copyright: copyrightLines(text), text };
    }
  }
  return { license: null, copyright: null, text: null };
}

// ---------------------------------------------------------------- Go: modules

function parseGoModRequires(goMod) {
  const reqs = [];
  for (const line of goMod.split('\n')) {
    const m = line.match(/^\s*(?:require\s+)?([a-z0-9.-]+\.[a-z]{2,}\/\S+)\s+(v\S+)/i);
    if (m) reqs.push({ module: m[1], version: m[2] });
  }
  return reqs.sort((a, b) => a.module.localeCompare(b.module));
}

function goModCache() {
  try {
    return execFileSync('go', ['env', 'GOMODCACHE'], { encoding: 'utf8' }).trim();
  } catch {
    return null;
  }
}

// The module cache escapes upper-case letters in paths as '!x'.
const cachePath = (module) => module.replace(/[A-Z]/g, (c) => `!${c.toLowerCase()}`);

// ---------------------------------------------------------------- Go: backend

function backendModules() {
  const goMod = read(path.join(ROOT, 'backend', 'go.mod'));
  const cache = goMod ? goModCache() : null;
  if (!cache) return [];

  return parseGoModRequires(goMod).map(({ module, version }) => {
    const dir = path.join(cache, `${cachePath(module)}@${version}`);
    const { license, copyright } = inspectDir(dir);
    return {
      module,
      version,
      license: license || NO_LICENSE_FILE,
      copyright: copyright || NO_COPYRIGHT,
      dir,
    };
  });
}

// ---------------------------------------------------------------- Go: isola-streamer

function streamerModules() {
  const modRoot = path.join(ROOT, 'browsers', 'isola-streamer');
  const goMod = read(path.join(modRoot, 'go.mod'));
  const cache = goMod ? goModCache() : null;
  if (!cache) return [];

  const out = [];
  for (const { module, version } of parseGoModRequires(goMod)) {
    const modDir = path.join(cache, `${cachePath(module)}@${version}`);
    const { license, copyright } = inspectDir(modDir);
    out.push({
      module,
      version,
      license: license || NO_LICENSE_FILE,
      copyright: copyright || NO_COPYRIGHT,
      dir: modDir,
    });
  }
  return out;
}

// ---------------------------------------------------------------- npm (frontend)

function npmComponents() {
  const lock = JSON.parse(read(path.join(ROOT, 'frontend', 'package-lock.json')) || '{}');
  const modules = path.join(ROOT, 'frontend', 'node_modules');
  const seen = new Map();

  for (const [key, entry] of Object.entries(lock.packages || {})) {
    if (key === '') continue;
    const dir = key ? path.join(ROOT, 'frontend', key) : modules;
    const pkgJson = read(path.join(dir, 'package.json'));
    let declared = entry.license || null;
    let author = null;
    if (pkgJson) {
      try {
        const j = JSON.parse(pkgJson);
        if (!declared) declared = j.license || null;
        author = typeof j.author === 'string' ? j.author : j.author?.name || null;
      } catch { /* ignore */ }
    }
    const name = entry.name || key.replace(/^.*node_modules\//, '');
    const version = entry.version || '—';
    const id = `${name}@${version}`;
    if (seen.has(id)) continue;

    const inspected = inspectDir(dir);
    const license = declared || inspected.license || NO_LICENSE_FILE;
    const copyright = inspected.copyright || (author ? `Copyright (c) ${author}` : NO_COPYRIGHT);
    seen.set(id, { name, version, license, copyright, dev: Boolean(entry.dev), dir });
  }

  const all = [...seen.values()].sort((a, b) => a.name.localeCompare(b.name));
  return {
    runtime: all.filter((p) => !p.dev),
    build: all.filter((p) => p.dev),
  };
}

// ---------------------------------------------------------------- desktop runner

const KNOWN_NPM_LICENSES = {
  electron: { license: 'MIT', copyright: 'Copyright (c) Electron contributors; Copyright (c) 2013-2020 GitHub Inc.' },
};

function electronRunner() {
  const dir = path.join(ROOT, 'browsers', 'electron-runner');
  const pkg = JSON.parse(read(path.join(dir, 'package.json')) || '{}');
  return Object.entries(pkg.dependencies || {}).map(([name, range]) => ({
    name,
    range,
    license: KNOWN_NPM_LICENSES[name]?.license || NO_LICENSE_FILE,
    copyright: KNOWN_NPM_LICENSES[name]?.copyright || NO_COPYRIGHT,
  }));
}

// ---------------------------------------------------------------- license texts

const SPDX_URL = {
  'Apache-2.0': 'https://www.apache.org/licenses/LICENSE-2.0',
  MIT: 'https://opensource.org/license/mit',
  ISC: 'https://opensource.org/license/isc-license-txt',
  'BSD-2-Clause': 'https://opensource.org/license/bsd-2-clause',
  'BSD-3-Clause': 'https://opensource.org/license/bsd-3-clause',
  'BlueOak-1.0.0': 'https://blueoakcouncil.org/license/1.0.0',
  'MIT-0': 'https://opensource.org/license/mit-0',
  'CC0-1.0': 'https://creativecommons.org/publicdomain/zero/1.0/legalcode',
  'CC-BY-4.0': 'https://creativecommons.org/licenses/by/4.0/legalcode',
  'Python-2.0': 'https://opensource.org/license/python-2.0',
};

// ---------------------------------------------------------------- render

const table = (rows, headers) => {
  const head = `| ${headers.join(' | ')} |\n|${headers.map(() => '---').join('|')}|\n`;
  return head + rows.map((r) => `| ${r.join(' | ')} |`).join('\n') + '\n';
};

function render() {
  const backend = backendModules();
  const streamer = streamerModules();
  const { runtime, build } = npmComponents();
  const runner = electronRunner();

  // Everything with a license file, for the full-text appendix.
  const texts = new Map(); // license -> text
  for (const c of [...backend, ...streamer, ...runtime, ...build]) {
    if (!c.dir || !c.license || texts.has(c.license)) continue;
    const t = inspectDir(c.dir).text;
    if (t) texts.set(c.license, t);
  }

  const goRows = (mods) => mods.map((m) => [`\`${m.module}\``, `\`${m.version}\``, m.license, m.copyright]);
  const npmRows = (mods) => mods.map((m) => [`\`${m.name}\``, `\`${m.version}\``, m.license, m.copyright]);

  const licenseCounts = (mods) => {
    const by = {};
    for (const m of mods) by[m.license] = (by[m.license] || 0) + 1;
    return Object.entries(by)
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .map(([l, n]) => [`${l}`, String(n)]);
  };

  const parts = [];
  parts.push(`# NOTICE

Isola
Copyright 2026 Isola contributors

This repository is licensed under the MIT License (see \`LICENSE\`).
It contains and builds upon third-party open-source software. The components
listed below are grouped by the part of the product they are used in, with the
version and license that the pinned build resolves to.

All listed components are distributed under permissive licenses (MIT, ISC,
BSD, Apache-2.0, and similar). No copyleft (GPL/LGPL/AGPL/MPL) component is
included.

Regenerate this file with \`node scripts/generate-notices.mjs\` after changing
dependencies.

In the tables below, the placeholder \`_(no license file in package — see SPDX id)_\`
means the package declares its license only in its metadata, and
\`_(not stated in license file)_\` means the upstream license file carries no
copyright-holder line (as with the Apache-2.0 boilerplate several projects
ship unedited).
`);

  const sizeLine = (mods) => {
    const counts = licenseCounts(mods);
    return counts.map(([l, n]) => `${l} × ${n}`).join(', ');
  };

  parts.push(`## 1. Backend — Go modules (\`backend/go.mod\`)

${backend.length} modules. License mix: ${sizeLine(backend)}.

${table(goRows(backend), ['Module', 'Version', 'License', 'Copyright'])}`);

  parts.push(`## 2. Isola streamer — Go modules (\`browsers/isola-streamer\`)

${streamer.length} modules. License mix: ${sizeLine(streamer)}.

${table(goRows(streamer), ['Module', 'Version', 'License', 'Copyright'])}`);

  parts.push(`## 3. Desktop runner (\`browsers/electron-runner\`)

The desktop runner is not lockfile-pinned; the versions below are the declared
ranges. Note that Electron itself bundles Chromium and Node.js, which carry
their own licenses (BSD/MIT/Apache and others) — see Electron's \`LICENSE\` and
Chromium's \`LICENSES.chromium.html\` for the full set.

${table(runner.map((r) => [`\`${r.name}\``, `\`${r.range}\``, r.license, r.copyright]), ['Package', 'Range', 'License', 'Copyright'])}`);

  parts.push(`## 4. Frontend — npm runtime dependencies

${runtime.length} packages bundled into the frontend build. License mix: ${sizeLine(runtime)}.

${table(npmRows(runtime), ['Package', 'Version', 'License', 'Copyright'])}`);

  parts.push(`## 5. Frontend — npm build/development dependencies

${build.length} packages used only to build, test and lint the frontend. They are
not redistributed in the production bundle, but are listed for completeness.
License mix: ${sizeLine(build)}.

${table(npmRows(build), ['Package', 'Version', 'License', 'Copyright'])}`);

  // Attribution notices required by upstream NOTICE files (Apache-2.0 §4(d)).
  const noticeFiles = [];
  const scanNotice = (dir) => {
    if (!dir) return;
    for (const e of (fs.existsSync(dir) ? fs.readdirSync(dir, { withFileTypes: true }) : [])) {
      if (e.isFile() && /^NOTICE/i.test(e.name)) noticeFiles.push(path.join(dir, e.name));
    }
  };
  scanNotice(backend.find((m) => m.module === 'gopkg.in/yaml.v3')?.dir);
  const attribution = noticeFiles
    .map((f) => {
      const body = read(f).trim();
      return `### \`${path.relative(ROOT, f)}\`\n\n\`\`\`text\n${body}\n\`\`\``;
    })
    .join('\n\n');

  parts.push(`## 6. Attribution notices from upstream NOTICE files

Apache-2.0 (§4 d) requires that attribution notices in an upstream NOTICE file
be preserved. The following are reproduced verbatim.

${attribution || '_None._'}
`);

  // Full license texts. Our own LICENSE supplies the MIT text; the other
  // licenses come from a dependency that actually carries them.
  const order = ['MIT', 'Apache-2.0', 'ISC', 'BSD-2-Clause', 'BSD-3-Clause'];
  const own = read(path.join(ROOT, 'LICENSE'));
  const textSections = [];
  for (const l of order) {
    const t = l === 'MIT' ? own : texts.get(l);
    if (t) textSections.push(`### ${l}\n\n\`\`\`text\n${t.trim()}\n\`\`\``);
  }

  const others = [...new Set([...backend, ...runtime, ...build].map((m) => m.license))]
    .filter((l) => !order.includes(l) && SPDX_URL[l])
    .sort();

  parts.push(`## 7. Full license texts

${textSections.join('\n\n')}

Additional licenses referenced by listed components (canonical text):

${others.map((l) => `- **${l}** — ${SPDX_URL[l]}`).join('\n') || '_None._'}
`);

  parts.push(`## 8. Not covered by this NOTICE

- **The Isola name and marks** are not covered by the MIT grant.

---

_Generated by \`scripts/generate-notices.mjs\`. Do not edit by hand._
`);

  return parts.join('\n') + '\n';
}

// ---------------------------------------------------------------- main

const output = render();
const check = process.argv.includes('--check');

if (check) {
  const existing = read(OUT);
  if (existing !== output) {
    console.error('NOTICE is out of date — run: node scripts/generate-notices.mjs');
    process.exit(1);
  }
  console.log('NOTICE is up to date.');
} else {
  fs.writeFileSync(OUT, output);
  console.log(`Wrote ${path.relative(ROOT, OUT)} (${output.split('\n').length} lines).`);
}
