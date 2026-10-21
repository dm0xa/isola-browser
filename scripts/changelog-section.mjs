#!/usr/bin/env node
// Prints one CHANGELOG.md section as a GitHub release body, so the published
// notes always match the tag. GitHub's own auto-generation lists merged pull
// requests, and this repository pushes straight to main — no PRs, so it could
// only ever emit a bare compare link.
//
//   node scripts/changelog-section.mjs v1.1.1              # body on stdout
//   node scripts/changelog-section.mjs v1.1.1 notes.md     # body to a file,
//                                                          # warnings on stdout
//
// Falls back to `## [Unreleased]` with an annotation when the tag has no
// section of its own, so tagging a version whose notes are still unreleased
// works while staying visible in the run log.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const CHANGELOG = path.join(ROOT, 'CHANGELOG.md');

const version = (process.argv[2] ?? '').trim().replace(/^v/, '');
const outPath = process.argv[3];

if (!version) {
  console.error('usage: node scripts/changelog-section.mjs <version> [output-file]');
  process.exit(2);
}

// Workflow commands are read from stdout, so only route warnings there when
// stdout is not the release body itself.
function warn(message) {
  if (outPath && process.env.GITHUB_ACTIONS === 'true') {
    console.log(`::warning::${message}`);
  } else {
    console.error(`warning: ${message}`);
  }
}

function emit(body) {
  if (outPath) {
    fs.writeFileSync(path.resolve(outPath), `${body}\n`);
  } else {
    console.log(body);
  }
}

const EMPTY = '_No changelog entry for this version._';

const text = fs.readFileSync(CHANGELOG, 'utf8');

// Keep a trailing block of link definitions ("[1.0.0]: https://…") out of the
// rendered notes; those are Markdown plumbing for the file, not release prose.
function stripLinkRefs(body) {
  return body.replace(/\n{2,}((?:\[[^\]]+\]:\s+\S+\n?)+\s*)$/, '').trim();
}

// A section runs from its `## [label]` heading to the next one; the text
// before the first heading is the introduction, not a section.
const sections = new Map();
for (const match of text.matchAll(/^## \[([^\]]+)\][^\n]*$/gm)) {
  const rest = text.slice(match.index + match[0].length);
  const end = rest.search(/^## \[/m);
  sections.set(match[1], stripLinkRefs(end === -1 ? rest : rest.slice(0, end)));
}

let label = version;
if (!sections.has(label)) {
  if (!sections.has('Unreleased')) {
    warn(`No CHANGELOG.md section for ${version} and no Unreleased section.`);
    emit(EMPTY);
    process.exit(0);
  }
  label = 'Unreleased';
  warn(`CHANGELOG.md has no "## [${version}]" section; publishing the "## [Unreleased]" notes instead.`);
}

const body = sections.get(label);
if (!body) {
  warn(`The "## [${label}]" section is empty.`);
  emit(EMPTY);
  process.exit(0);
}

emit(body);
