// The first/third-party line the streamer draws (`tp`, cookie domains, tracker
// labels) is the registrable domain, so the graph must use the same one: a host
// folded into a page card here must fold in browsers/isola-streamer/graph.go
// too. Port of registrableDomain() in browsers/isola-streamer/trackers.go —
// keep the suffix list in step with it.
//
// Compact, not the full Public Suffix List: it covers the multi-label endings
// that appear in captured traffic. A single-label TLD needs no entry, because
// the last two labels already are the registrable domain.

const MULTI_LABEL_SUFFIXES: Record<string, true> = {
	'ac.uk': true, 'co.uk': true, 'gov.uk': true, 'ltd.uk': true, 'me.uk': true,
	'net.uk': true, 'nhs.uk': true, 'org.uk': true, 'plc.uk': true, 'police.uk': true,
	'com.au': true, 'net.au': true, 'org.au': true, 'edu.au': true, 'gov.au': true,
	'co.nz': true, 'net.nz': true, 'org.nz': true, 'co.za': true, 'org.za': true,
	'net.za': true, 'co.jp': true, 'ne.jp': true, 'or.jp': true, 'co.kr': true,
	'or.kr': true, 'com.br': true, 'net.br': true, 'org.br': true, 'com.mx': true,
	'com.ar': true, 'com.tr': true, 'com.cn': true, 'com.hk': true, 'com.sg': true,
	'com.my': true, 'com.tw': true, 'com.ua': true, 'co.in': true, 'net.in': true,
	'org.in': true, 'co.il': true, 'org.il': true, 'com.se': true, 'org.se': true,
	'net.se': true, 'pp.se': true, 'com.no': true, 'org.no': true, 'net.no': true,
	'com.dk': true, 'org.dk': true, 'net.dk': true, 'com.fi': true, 'org.fi': true,
	'net.fi': true, 'com.pl': true, 'org.pl': true, 'net.pl': true, 'com.es': true,
	'org.es': true, 'net.es': true, 'com.fr': true, 'org.fr': true, 'net.fr': true,
	'com.de': true, 'org.de': true, 'net.de': true, 'com.nl': true, 'org.nl': true,
	'net.nl': true, 'co.at': true, 'or.at': true, 'co.ch': true, 'org.ch': true,
	'com.pt': true, 'com.gr': true, 'com.ro': true, 'com.cz': true, 'com.ru': true,
	'org.ru': true, 'net.ru': true, 'co.id': true, 'or.id': true, 'co.th': true,
	'in.th': true,
};

function isIPv4Literal(host: string): boolean {
  const parts = host.split('.');
  if (parts.length !== 4) return false;
  return parts.every((p) => p !== '' && p.length <= 3 && /^[0-9]+$/.test(p));
}

/**
 * eTLD+1 for a host: `a.b.example.co.uk` → `example.co.uk`. IP literals and
 * single-label hosts come back unchanged, so two unrelated bare hostnames are
 * never treated as one site.
 */
export function registrableDomain(host: string): string {
  const h = host.trim().toLowerCase().replace(/\.$/, '');
  if (h === '') return '';
  if (/[:[\]]/.test(h) || isIPv4Literal(h)) return h;
  const parts = h.split('.');
  if (parts.length <= 2) return h;
  const lastTwo = `${parts[parts.length - 2]}.${parts[parts.length - 1]}`;
  if (Object.hasOwn(MULTI_LABEL_SUFFIXES, lastTwo) && parts.length >= 3) {
    return `${parts[parts.length - 3]}.${lastTwo}`;
  }
  return lastTwo;
}
