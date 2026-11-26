// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

const $ = id => document.getElementById(id);
const form = $('search');
const query = $('query');
const cdxLimit = 100;
let active;
let debounce;

const playbackBase = fetch('../search/status')
  .then(response => response.ok ? response.json() : {})
  .then(settings => settings.playbackUrlBase)
  .catch(() => undefined);

function resetSearch(focus = true) {
  clearTimeout(debounce);
  active?.abort();
  active = undefined;
  form.reset();
  $('offset').value = '0';
  form.querySelector('details').open = false;
  const cdx = $('mode').value === 'cdx';
  $('cdx-filters').hidden = $('cdx-filters').disabled = !cdx;
  $('text-filters').hidden = $('text-filters').disabled = cdx;
  query.name = cdx ? 'url' : 'q';
  query.placeholder = cdx ? 'https://example.org/' : 'Find a word, a story, a moment…';
  $('query-label').textContent = cdx ? 'Capture URL' : 'Search the archive';
  $('inspection').close();
  $('provenance').replaceChildren();
  $('timeline').hidden = $('pagination').hidden = cdx;
  $('ticks').replaceChildren();
  $('range').textContent = 'Awaiting search';
  $('count').textContent = '0 CAPTURES';
  $('start').textContent = $('end').textContent = '—';
  $('previous').disabled = $('next').disabled = true;
  $('page-position').textContent = '';
  $('cdx-count').hidden = true;
  $('results').replaceChildren();
  if (!cdx) $('results').append($('empty-text').content.cloneNode(true));
  $('status').textContent = 'READY';
  if (focus) query.focus();
}

function element(tag, text) {
  const node = document.createElement(tag);
  node.textContent = text;
  return node;
}

function inspect(fields, raw) {
  const list = $('provenance');
  list.replaceChildren();
  if (raw !== undefined) {
    list.append(element('dt', 'CDX record'), element('dd', raw));
  } else {
    for (const [key, value] of Object.entries(fields)) {
      list.append(element('dt', key), element('dd', value));
    }
  }
  $('inspection').showModal();
}

function replayTimestamp(value) {
  if (!value) return '';
  if (/^\d{14}$/.test(value)) return value;
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toISOString().slice(0, 19).replace(/\D/g, '') : '';
}

async function linkReplayTitle(title, url, stamp) {
  if (!stamp || !/^https?:\/\//i.test(url) || !URL.canParse(url)) return;
  const base = await playbackBase;
  if (!base || !title.isConnected) return;
  const link = element('a', title.textContent);
  link.href = base + '/' + stamp + '/' + url;
  title.replaceChildren(link);
}

function renderCapture(fields, cdx, raw) {
  const row = document.createElement('article');
  row.className = 'result';
  const content = document.createElement('div');
  const title = element('h2', cdx ? fields.url : fields.title || fields.url);
  content.append(title);
  if (!cdx && fields.title) content.append(element('p', fields.url));
  const time = element('time', fields.timestamp);
  if (!cdx) time.dateTime = fields.timestamp;
  content.append(time);
  if (cdx) content.append(element('p', 'HTTP status: ' + (fields.status || '—') + ' · MIME: ' + (fields.mime || '—')));
  else if (fields.snippet) content.append(element('p', fields.snippet));

  const actions = document.createElement('div');
  actions.className = 'result-actions';
  const button = element('button', 'Metadata');
  button.onclick = () => inspect(fields, raw);
  actions.append(button);
  row.append(content, actions);
  $('results').append(row);
  const stamp = cdx ? fields.timestamp : replayTimestamp(fields.pageTimestamp) || replayTimestamp(fields.timestamp);
  let target = fields.url;
  if (cdx && (fields.method === 'POST' || fields.method === 'PUT')) {
    target = target.split('#')[0];
    target += (target.includes('?') ? '&' : '?') + '__wb_method=' + fields.method;
    if (fields.requestBody) target += '&' + fields.requestBody;
  }
  if (/^\d{14}$/.test(stamp)) linkReplayTitle(title, target, stamp);
}

function renderText(data) {
  const hits = data.hits || [];
  $('previous').disabled = data.offset === 0;
  $('next').disabled = data.offset + data.limit >= Math.min(data.totalEstimate, 10000);
  $('page-position').textContent = hits.length
    ? (data.offset + 1) + '–' + (data.offset + hits.length) + ' / ~' + data.totalEstimate : '';
  $('count').textContent = hits.length + ' DISPLAYED';
  $('range').textContent = data.totalEstimate + ' MATCHES · ESTIMATE';

  const {counts, from, to, bounds} = data.timeline;
  $('start').textContent = counts.length ? from.slice(0, 10) : '—';
  $('end').textContent = counts.length ? to.slice(0, 10) : '—';
  $('ticks').replaceChildren();
  const peak = Math.max(1, ...counts);
  counts.forEach((count, i) => {
    const marker = document.createElement('button');
    marker.type = 'button';
    marker.disabled = !count;
    marker.style.height = (count ? 8 + 26 * Math.log1p(count) / Math.log1p(peak) : 2) + 'px';
    marker.title = count ? count + ' matches in this interval — click to filter' : 'No matching captures in this interval';
    marker.setAttribute('aria-label', marker.title);
    marker.onclick = () => {
      $('filter-from').value = bounds[i];
      $('filter-to').value = bounds[i + 1];
      $('offset').value = '0';
      form.requestSubmit();
    };
    $('ticks').append(marker);
  });
  for (const hit of hits) renderCapture(hit.fields, false);
  if (!hits.length) $('results').textContent = 'No matching text captures.';
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  clearTimeout(debounce);
  active?.abort();
  const request = active = new AbortController();
  const cdx = $('mode').value === 'cdx';
  const params = new URLSearchParams(new FormData(form));
  if (cdx) {
    params.set('output', 'json');
    params.set('limit', cdxLimit);
  }
  $('status').textContent = 'SEARCHING';
  try {
    const response = await fetch((cdx ? '../search/cdx?' : '../search?') + params, {signal: request.signal});
    if (!response.ok) throw new Error(await response.text());
    const data = cdx ? await response.text() : await response.json();
    if (active !== request) return;
    $('results').replaceChildren();
    if (cdx) {
      const rows = data.trim() ? data.trim().split('\n') : [];
      const terminal = JSON.parse(rows.pop() || 'null');
      if (!Array.isArray(terminal) || terminal.length !== 1 || typeof terminal[0]?.truncated !== 'boolean') {
        throw new Error('Search ended before completion was confirmed.');
      }
      const [summary] = terminal;
      if (summary.error) throw new Error(summary.error);
      for (const line of rows) renderCapture(JSON.parse(line), true, line);
      $('cdx-count').textContent = rows.length + (rows.length === 1 ? ' capture' : ' captures') + ' returned (limit ' + cdxLimit + ')';
      $('cdx-count').hidden = false;
      if (summary.overflowSkipped) $('cdx-count').textContent += ' · Overflow keys excluded';
      if (summary.truncated) {
        $('cdx-count').textContent += ' · Search limit reached after scanning ' + summary.scanned + ' captures. Narrow the query.';
        $('status').textContent = 'PARTIAL RESULTS';
        return;
      }
      if (!rows.length) $('results').textContent = 'No matching captures.';
    } else {
      renderText(data);
    }
    $('status').textContent = 'READY';
  } catch (error) {
    if (active !== request) return;
    $('cdx-count').hidden = true;
    $('status').textContent = 'SEARCH FAILED';
    $('results').textContent = error.message;
  }
});

$('clear-time').onclick = () => {
  $('filter-from').value = $('filter-to').value = '';
  $('offset').value = '0';
  form.requestSubmit();
};
function changePage(delta) {
  $('offset').value = Math.max(0, Number($('offset').value) + delta);
  form.requestSubmit();
}
$('previous').onclick = () => changePage(-20);
$('next').onclick = () => changePage(20);
query.addEventListener('input', () => {
  $('offset').value = '0';
  clearTimeout(debounce);
  if ($('mode').value === 'text' && query.value.trim()) debounce = setTimeout(() => form.requestSubmit(), 300);
});
$('reset-search').onclick = () => resetSearch();
$('mode').onchange = () => resetSearch();
resetSearch(false);
