// loader.js — resolve `--adapter <name|path>` to a module and instantiate it.
//
// Builtin short names map into ../adapters/. Anything else is treated as a path
// or a module specifier, so a real deployment ships its own adapter file and
// changes nothing else in this repo.
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { assertAdapter } from '../adapters/adapter.js';
import { CliError, EXIT } from './config.js';

const BUILTIN = {
  memory: '../adapters/memory-adapter.js',
  http: '../adapters/http-adapter.js',
};

/** `--adapter-opt a=1 --adapter-opt auth.token=x` -> {a: '1', auth: {token: 'x'}} */
export function parseAdapterOptions(raw) {
  const out = {};
  const list = raw == null ? [] : (Array.isArray(raw) ? raw : [raw]);
  for (const item of list) {
    const s = String(item);
    const eq = s.indexOf('=');
    if (eq === -1) { out[s] = true; continue; }
    const keyPath = s.slice(0, eq).split('.');
    let v = s.slice(eq + 1);
    if (v === 'true') v = true;
    else if (v === 'false') v = false;
    else if (v === 'null') v = null;
    let cur = out;
    for (let i = 0; i < keyPath.length - 1; i++) {
      if (typeof cur[keyPath[i]] !== 'object' || cur[keyPath[i]] === null) cur[keyPath[i]] = {};
      cur = cur[keyPath[i]];
    }
    cur[keyPath[keyPath.length - 1]] = v;
  }
  return out;
}

export async function loadAdapter(spec, options = {}) {
  if (!spec) throw new CliError('notConfigured', 'no adapter: pass --adapter memory|http|<path-to-module>', EXIT.NOT_CONFIGURED);
  const builtin = BUILTIN[spec];
  let url;
  if (builtin) {
    url = new URL(builtin, import.meta.url).href;
  } else if (spec.startsWith('.') || spec.startsWith('/')) {
    url = pathToFileURL(path.resolve(spec)).href;
  } else {
    url = spec; // bare specifier — let Node resolve it
  }
  let mod;
  try {
    mod = await import(url);
  } catch (e) {
    throw new CliError('adapterLoadFailed', `could not load adapter "${spec}": ${e.message}`, EXIT.NOT_CONFIGURED);
  }
  const factory = mod.default || mod.createAdapter;
  if (typeof factory !== 'function') {
    throw new CliError('adapterLoadFailed', `adapter "${spec}" must default-export createAdapter(options)`, EXIT.NOT_CONFIGURED);
  }
  return assertAdapter(await factory(options), `adapter "${spec}"`);
}
