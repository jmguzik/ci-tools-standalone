(() => {
  'use strict';
  const maxParts = 5000, maxText = 250000, maxLines = 5000, maxLine = 4000;
  const str = value => typeof value === 'string' ? value : '';
  const contextPattern = '(?:app\\.ci|core-ci|build(?:0[1-9]|[1-9][0-9])|hosted-mgmt2?)';
  const contextExact = new RegExp('^' + contextPattern + '$', 'i');
  const contexts = new RegExp('\\b' + contextPattern + '(?![\\w.-])', 'gi');
  const countRow = new RegExp('^(' + contextPattern + ')(?![\\w.-])\\s*(?::|[—–-])\\s*(\\d+)(?!\\d|[,.]\\d)([\\s\\S]*)$', 'i');
  const inlineRow = new RegExp('(?:^|;\\s*|,\\s*|\\band\\s+)(' + contextPattern + ')(?![\\w.-])\\s+(\\d+)(?!\\d|[,.]\\d)(?=\\s|[,.]|$)', 'gi');
  const unavailable = /authentication\s+(?:failed|denied|error|failures?)|rejected(?: both)? authentication|could not be inspected|(?:access|permission) denied|uninspected|not inspected|unavailable|workload state is unknown/i;
  const sectionHeading = /^(?:priority checks|additional blockers|scheduling\s*\/\s*startup blockers|persistent dependency failures|monitor\s*\/\s*lower-priority snapshot|other scan observations|(?:other\s+)?(?:pending(?:[- ]pod|[- ]workload)?\s+)?snapshot(?:\s+totals?)?|pending(?:[- ]pod|[- ]workload)?\s+(?:scan|snapshot|counts|pods))(?:\s*[:—–-].*)?$/i;

  function task(occurrence) {
    const explicit = str(occurrence?.message?.scheduled_task).trim();
    if (explicit) return explicit;
    const match = (str(occurrence?.message?.title).slice(0, maxLine) + '\n' + str(occurrence?.message?.text).slice(0, maxLine)).match(/\[Scheduled task:\s*([^\]\n]+)\]/i);
    return match ? match[1].split('|')[0].trim().replace(/^`+|`+$/g, '') : '';
  }
  function time(occurrence) {
    const value = Date.parse(str(occurrence?.message?.reported_at));
    return Number.isFinite(value) ? value : 0;
  }
  function order(a, b) {
    return time(a) - time(b) || str(a.identity?.id).localeCompare(str(b.identity?.id)) || str(a.message?.text).localeCompare(str(b.message?.text));
  }
  /** runs combines only backend-validated run identities, retaining every original root and source. */
  function runs(occurrences) {
    if (!Array.isArray(occurrences)) return [];
    const groups = new Map(), seen = new Set();
    const input = occurrences.filter(o => o && typeof o === 'object' && o.message && typeof o.message === 'object').sort(order).slice(-maxParts);
    for (const occurrence of input) {
      const message = occurrence.message, id = str(occurrence.identity?.id);
      const identity = [task(occurrence), str(occurrence.identity?.channel), str(message.source)];
      const duplicate = JSON.stringify([...identity, id]);
      if (id && seen.has(duplicate)) continue;
      if (id) seen.add(duplicate);
      const run = str(message.scheduled_run);
      const trustedKey = run.length > 0 && run.length <= 512 && !/[\x00-\x20\x7f]/.test(run) && identity.every(value => value.length > 0);
      const key = trustedKey ? JSON.stringify([...identity, run]) : Symbol();
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(occurrence);
    }
    return [...groups.values()].map(parts => combine(parts, occurrences.length > maxParts)).sort((a, b) => order(b, a));
  }

  function combine(parts, limited) {
    let combined = '';
    for (const part of parts) {
      const source = (combined ? '\n\n' : '') + str(part.message.text);
      const remaining = maxText - combined.length;
      combined += source.slice(0, remaining);
      if (source.length > remaining) limited = true;
    }
    return {...parts[0], message: {...parts[0].message, text: combined}, parts, extraction_limited: limited};
  }
  function plain(line) {
    // Formatting may wrap a context, but HTML, links and inline examples cannot supply metrics.
    return line.replace(/<https?:\/\/[^>|]+\|([^>]+)>/g, '$1').replace(/\[([^\]]+)\]\(https?:\/\/[^)]+\)/g, '$1').replace(/[`*_]/g, '').replace(/^\s*(?:[•+-]\s+|\d+[.)]\s+)/, '').trim();
  }
  function safeCount(value) {
    const parsed = Number(value);
    return /^\d+$/.test(value) && Number.isSafeInteger(parsed) && parsed >= 0 ? parsed : null;
  }
  /** pending extracts explicit context-level Pending pod snapshots; prose remains the authority. */
  function pending(report) {
    const result = {observations: [], partial: report?.extraction_limited === true};
    if (task(report) !== 'pending_workloads_scan') return result;
    const originals = Array.isArray(report.parts) ? report.parts : [report];
    const facts = new Map(), qualifiers = new Set();
    if (originals.length > maxParts) result.partial = true;
    let scoped = false, section = '', fence = '', remainingText = maxText, remainingLines = maxLines;
    for (const original of originals.slice(0, maxParts)) {
      const source = str(original?.message?.text);
      if (!remainingText || !remainingLines) { result.partial = true; break; }
      if (source.length > remainingText) result.partial = true;
      const bounded = source.slice(0, remainingText);
      remainingText -= bounded.length;
      const complete = source.length > bounded.length ? bounded.slice(0, Math.max(0, bounded.lastIndexOf('\n'))) : bounded;
      const lines = complete.replace(/\r\n?/g, '\n').split('\n');
      if (lines.length > remainingLines) result.partial = true;
      const boundedLines = lines.slice(0, remainingLines);
      remainingLines -= boundedLines.length;
      for (const raw of boundedLines) {
        if (raw.length > maxLine) { result.partial = true; continue; }
        const fenceMatch = raw.match(/^\s*(`{3,}|~{3,})/);
        if (fenceMatch) { if (!fence) fence = fenceMatch[1]; else if (fenceMatch[1][0] === fence[0] && fenceMatch[1].length >= fence.length) fence = ''; continue; }
        if (fence || /^\s*\[[^\]\n]+\]\(https?:\/\/[^)]+\)\s*$/.test(raw) || /^\s*<https?:\/\/[^>|]+\|[^>]+>\s*$/.test(raw) || /^\s*`+[^`]+`+\s*$/.test(raw) || /^\s*(?:>|&gt;)/.test(raw) || /\b(?:example|sample)\s*:\s*[`"']/i.test(raw)) continue;
        if (!raw.trim()) continue;
        if (/<\/?(?:script|img|iframe|div|span|table|svg|pre|code)\b/i.test(raw)) { result.partial = true; continue; }
        const line = plain(raw).replace(/^#{1,6}\s+/, '');
        if (!line || /^\[Scheduled task:/i.test(line)) continue;
        const qualifier = raw.match(/Counts?\s+(?:include\b[^.]*|below\b[^.]*snapshots?[^.]*)/i);
        if (qualifier && qualifiers.size < 100) qualifiers.add(qualifier[0]);
        const found = [...line.matchAll(contexts)].map(match => ({name: match[0].toLowerCase(), index: match.index}));
        if (/\b(?:example|sample|e\.g\.)\s*:/i.test(line) && !countRow.test(line)) { if (found.length) result.partial = true; continue; }
        if (sectionHeading.test(line)) { scoped = true; section = raw.trim(); }
        // Snapshot totals inline are explicit scope, and may contain several context/count pairs.
        const inline = line.match(/^(?:Other\s+)?(?:Pending\s+)?snapshot totals\s*:\s*(.*)$/i);
        let candidates = [];
        if (inline) {
          scoped = true; section = line.slice(0, line.indexOf(':'));
          for (const match of inline[1].matchAll(inlineRow)) {
            const suffix = inline[1].slice(match.index + match[0].length);
            if (countSuffix(suffix) || new RegExp('^\\s+and\\s+' + contextPattern + '\\s+\\d', 'i').test(suffix)) candidates.push({context: match[1].toLowerCase(), digits: match[2]});
            else result.partial = true;
          }
        } else {
          // Semicolon-separated compact rows have a shared, explicit Pending metric.
          const compactScope = /;/.test(line) && line.split(/;/).some(chunk => {const row = chunk.trim().match(countRow); return row && pendingUnit(row[3]);});
          const chunks = line.split(/;\s*(?=(?:app\.ci|core-ci|build\d\d|hosted-mgmt2?)\s*[:—–-])/i);
          for (const chunk of chunks) {
            const match = chunk.match(countRow);
            if (!match) continue;
            const suffix = match[3];
            const explicit = pendingUnit(suffix);
            const cleanSuffix = countSuffix(suffix);
            if (explicit || ((scoped || compactScope) && cleanSuffix)) candidates.push({context: match[1].toLowerCase(), digits: match[2]});
          }
          const zero = line.match(new RegExp('^(' + contextPattern + ')\\s*(?::|[—–-])\\s*(?:no|zero)(\\s+Pending.*)$', 'i'));
          if (zero && pendingUnit(zero[2])) candidates.push({context: zero[1].toLowerCase(), digits: '0'});
        }
        const handled = new Set();
        for (const candidate of candidates) {
          const value = safeCount(candidate.digits);
          if (value === null) { result.partial = true; continue; }
          addFact(facts, candidate.context, value, 'observed', raw.trim(), section);
          handled.add(candidate.context);
        }
        if (unavailable.test(line)) {
          for (const context of found) {
            // Associate access statements with their own clause, never every context in the paragraph.
            const clause = line.slice(context.index).split(/;|\.\s+(?=[A-Z])/)[0];
            if (!unavailable.test(clause)) continue;
            addFact(facts, context.name, null, 'unavailable', raw.trim(), section);
            handled.add(context.name); result.partial = true;
          }
        }
        if (found.some(context => !handled.has(context.name)) && !sectionHeading.test(line)) result.partial = true;
        if (/\b(?:build\d+|[a-z][a-z0-9-]*-ci|hosted-mgmt\d+)\b\s*[:—–-]\s*\d/i.test(line) && !candidates.length) result.partial = true;
        if (/^[a-z][a-z0-9.-]*\s*[:—–-]\s*\d+\s+Pending\b/i.test(line) && !candidates.length) result.partial = true;
        // Unknown headings end inherited scope; explicit metric rows remain independently parseable.
        if (!found.length && !sectionHeading.test(line) && (/^(?:#{1,6}\s|\*\*|\*[^*])/.test(raw.trim()) || /^[a-z][a-z /-]{1,80}:?$/i.test(line))) { scoped = false; section = ''; }
      }
    }
    if (fence) result.partial = true;
    for (const fact of facts.values()) for (const qualifier of qualifiers) if (!fact.notes.includes(qualifier)) fact.notes.push(qualifier);
    result.observations = [...facts.values()].sort((a, b) => a.context.localeCompare(b.context));
    if (!result.observations.length || result.observations.some(fact => fact.coverage !== 'observed')) result.partial = true;
    return result;
  }
  function pendingUnit(suffix) {
    return /^\s+Pending(?:\s+pods?)?(?=\s*(?:$|[:.,;(])|\s+(?:observed|reported|at\s+(?:the\s+)?(?:(?:first|initial|scan)\s+)?snapshot)\b)/i.test(suffix);
  }
  function countSuffix(suffix) {
    return /^\s*$/.test(suffix) || /^\s*[:.,;(]/.test(suffix) || /^\s+across\b/i.test(suffix) || pendingUnit(suffix);
  }
  function addFact(facts, context, value, coverage, evidence, section) {
    if (!contextExact.test(context)) return;
    let fact = facts.get(context);
    if (!fact) {
      fact = {context, value, coverage, evidence: [], notes: []}; facts.set(context, fact);
    } else if (fact.coverage !== coverage || fact.value !== value) {
      fact.value = null; fact.coverage = 'conflict';
      if (!fact.notes.includes('Conflicting observations; no count selected.')) fact.notes.push('Conflicting observations; no count selected.');
    }
    if (!fact.evidence.includes(evidence)) fact.evidence.push(evidence);
    if (section && !fact.notes.includes(section)) fact.notes.push(section);
  }
  globalThis.OpsReports = {runs, pending};
})();
