const $ = id => document.getElementById(id);
let active;
function inspect(hit) {
  const list = $('provenance'); list.replaceChildren();
  for (const [key, value] of Object.entries(hit.fields || {})) {
    const term = document.createElement('dt'), detail = document.createElement('dd');
    term.textContent = key; detail.textContent = String(value); list.append(term, detail);
  }
  $('inspection').showModal();
}
$('search').addEventListener('submit', async event => {
  event.preventDefault(); clearTimeout(debounce); active?.abort(); const request = active = new AbortController();
  $('status').textContent = 'SEARCHING';
  try {
    const params = new URLSearchParams(new FormData(event.currentTarget));
    const response = await fetch('search?' + params, {signal:request.signal});
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    if(active!==request)return;
    $('previous').disabled=data.offset===0; $('next').disabled=data.offset+data.limit>=Math.min(data.totalEstimate,10000); $('page-position').textContent=`${data.offset+1}–${data.offset+data.hits.length} / ~${data.totalEstimate}`;
    const hits = data.hits || []; $('results').replaceChildren(); $('ticks').replaceChildren();
    $('count').textContent = hits.length + ' DISPLAYED'; $('range').textContent = data.totalEstimate + ' MATCHES · ESTIMATE';
    const chronological = [...hits].sort((a,b) => String(a.fields.timestamp).localeCompare(String(b.fields.timestamp)));
    $('start').textContent = chronological[0]?.fields.timestamp?.slice(0,10) || '—';
    $('end').textContent = chronological.at(-1)?.fields.timestamp?.slice(0,10) || '—';
    // Bin by elapsed time, not result rank. Height shows capture density.
    const dates = chronological.map(hit => Date.parse(hit.fields.timestamp));
    const valid = dates.filter(Number.isFinite);
    const first = Date.parse(data.timeline?.from) || Math.min(...valid), last = Date.parse(data.timeline?.to) || Math.max(...valid);
    $('start').textContent = data.timeline?.from?.slice(0,10) || '—'; $('end').textContent = data.timeline?.to?.slice(0,10) || '—';
    const bins = Array.from({length:48}, () => []);
    chronological.forEach((hit, index) => {
      if (!Number.isFinite(dates[index])) return;
      const position = last === first ? 24 : Math.min(47, Math.floor((dates[index]-first)/(last-first)*48));
      bins[position].push(hit);
    });
    const counts = data.timeline?.counts || bins.map(bin => bin.length);
    const peak = Math.max(1, ...counts);
    for (const [position,bin] of bins.entries()) {
      const marker = document.createElement('button'); marker.type = 'button';
      const count = counts[position] || 0;
      marker.disabled = !count;
      marker.style.height = (count ? 8 + 26 * Math.log1p(count) / Math.log1p(peak) : 2) + 'px';
      marker.title = count ? `${count} matches in this interval — click to filter` : 'No matching captures in this interval';
      marker.setAttribute('aria-label', marker.title);
      marker.onclick = () => {
        const span = (last-first)/48;
        const from = new Date(first + position*span).toISOString();
        const to = new Date(first === last ? last+1 : first+(position+1)*span+1).toISOString();
        $('filter-from').value=from; $('filter-to').value=to; $('offset').value='0';
        $('search').requestSubmit();
      };
      $('ticks').append(marker);
    }
    for (const hit of hits) {
      const row = document.createElement('article'); row.className = 'result';
      const content = document.createElement('div'), title = document.createElement('h2'), url = document.createElement('p'), time = document.createElement('time'), button = document.createElement('button');
      title.textContent = hit.fields.title || hit.fields.url; url.textContent = hit.fields.url;
      time.textContent = hit.fields.timestamp; time.dateTime = hit.fields.timestamp;
      button.textContent = 'INSPECT ↗'; button.onclick = () => inspect(hit);
      const excerpt = document.createElement('p'); excerpt.textContent = hit.fields.snippet || '';
      const actions = document.createElement('div'); actions.className = 'result-actions';
      actions.append(button);
      content.append(title,url,time,excerpt); row.append(content,actions); $('results').append(row);
    }
    if (!hits.length) $('results').textContent = 'No matching text captures.';
    $('status').textContent = 'READY';
  } catch(error) {
    if (error.name !== 'AbortError') { $('status').textContent = 'SEARCH FAILED'; $('results').textContent = error.message; }
  }
});

$('clear-time').onclick=()=>{$('filter-from').value='';$('filter-to').value='';$('search').requestSubmit();};

$('previous').onclick=()=>{$('offset').value=Math.max(0,Number($('offset').value)-20);$('search').requestSubmit();};
$('next').onclick=()=>{$('offset').value=Number($('offset').value)+20;$('search').requestSubmit();};
let debounce;
$('query').addEventListener('input',()=>{$('offset').value='0';clearTimeout(debounce);if($('query').value.trim())debounce=setTimeout(()=>$('search').requestSubmit(),300);});
