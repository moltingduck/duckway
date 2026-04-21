/**
 * Duckway Smart Search
 *
 * Supports:
 * - Plain text search (matches any column)
 * - Filter syntax: field:value (e.g., service:openai status:active)
 * - Multiple filters: service:openai client:laptop
 * - Quoted values: name:"my key"
 * - Autocomplete dropdown with suggestions
 */

function initSmartSearch(config) {
  // config: { inputId, targetId (tbody or card container), type: 'table'|'cards',
  //           filters: [{key, label, values: fn or array}], countId }

  var input = document.getElementById(config.inputId);
  if (!input) return;

  var dropdown = document.createElement('div');
  dropdown.className = 'search-dropdown';
  dropdown.style.display = 'none';
  input.parentNode.style.position = 'relative';
  input.parentNode.appendChild(dropdown);

  // Hint
  var hint = document.createElement('div');
  hint.className = 'search-hint';
  hint.innerHTML = config.filters.map(function(f) {
    return '<code>' + f.key + ':</code>';
  }).join(' ') + ' <span class="text-muted">or type to search all</span>';
  input.parentNode.appendChild(hint);

  var lastQuery = '';

  input.addEventListener('input', function() {
    var val = input.value;
    lastQuery = val;
    applyFilter(config, val);
    showSuggestions(config, dropdown, val, input);
  });

  input.addEventListener('focus', function() {
    if (input.value === '') showSuggestions(config, dropdown, '', input);
  });

  input.addEventListener('keydown', function(e) {
    var items = dropdown.querySelectorAll('.search-suggest-item');
    var active = dropdown.querySelector('.search-suggest-item.active');

    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (!active && items.length) { items[0].classList.add('active'); }
      else if (active && active.nextElementSibling) {
        active.classList.remove('active');
        active.nextElementSibling.classList.add('active');
      }
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (active && active.previousElementSibling) {
        active.classList.remove('active');
        active.previousElementSibling.classList.add('active');
      }
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (active) { selectSuggestion(input, active.dataset.insert, config, dropdown); }
    } else if (e.key === 'Escape') {
      dropdown.style.display = 'none';
    }
  });

  document.addEventListener('click', function(e) {
    if (!input.contains(e.target) && !dropdown.contains(e.target)) {
      dropdown.style.display = 'none';
    }
  });
}

function showSuggestions(config, dropdown, query, input) {
  dropdown.innerHTML = '';
  var suggestions = getSuggestions(config, query);

  if (suggestions.length === 0) {
    dropdown.style.display = 'none';
    return;
  }

  suggestions.forEach(function(s) {
    var item = document.createElement('div');
    item.className = 'search-suggest-item';
    item.innerHTML = s.html;
    item.dataset.insert = s.insert;
    item.addEventListener('click', function() {
      selectSuggestion(input, s.insert, config, dropdown);
    });
    dropdown.appendChild(item);
  });

  dropdown.style.display = 'block';
}

function getSuggestions(config, query) {
  var suggestions = [];
  var parts = query.split(/\s+/);
  var lastPart = parts[parts.length - 1] || '';
  var prefix = parts.slice(0, -1).join(' ');
  if (prefix) prefix += ' ';

  // If typing a filter key (before colon)
  if (!lastPart.includes(':')) {
    config.filters.forEach(function(f) {
      if (!lastPart || f.key.startsWith(lastPart.toLowerCase())) {
        suggestions.push({
          html: '<strong>' + f.key + ':</strong> <span class="text-muted">' + f.label + '</span>',
          insert: prefix + f.key + ':'
        });
      }
    });
    return suggestions.slice(0, 8);
  }

  // If typing a filter value (after colon)
  var colonIdx = lastPart.indexOf(':');
  var filterKey = lastPart.substring(0, colonIdx).toLowerCase();
  var filterVal = lastPart.substring(colonIdx + 1).toLowerCase();

  var filterDef = config.filters.find(function(f) { return f.key === filterKey; });
  if (!filterDef) return [];

  var values = typeof filterDef.values === 'function' ? filterDef.values() : (filterDef.values || []);
  values.forEach(function(v) {
    if (!filterVal || v.toLowerCase().startsWith(filterVal)) {
      var needsQuote = v.includes(' ');
      var insertVal = needsQuote ? '"' + v + '"' : v;
      suggestions.push({
        html: '<code>' + filterKey + ':</code><strong>' + v + '</strong>',
        insert: prefix + filterKey + ':' + insertVal
      });
    }
  });

  return suggestions.slice(0, 10);
}

function selectSuggestion(input, insert, config, dropdown) {
  input.value = insert;
  dropdown.style.display = 'none';
  input.focus();
  applyFilter(config, insert);
}

function applyFilter(config, query) {
  var parsed = parseQuery(query);
  var target = document.getElementById(config.targetId);
  if (!target) return;

  var elements = config.type === 'cards'
    ? target.querySelectorAll('[id^="client-card-"]')
    : target.querySelectorAll('tr');

  var visible = 0;
  var total = elements.length;

  elements.forEach(function(el) {
    var text = el.textContent.toLowerCase();
    var show = true;

    // Check each filter
    for (var key in parsed.filters) {
      var val = parsed.filters[key].toLowerCase();
      // Get the specific column/field value if possible
      var colText = getFieldText(el, key, config);
      if (colText !== null) {
        if (colText.toLowerCase().indexOf(val) < 0) { show = false; break; }
      } else {
        if (text.indexOf(val) < 0) { show = false; break; }
      }
    }

    // Check free text
    if (show && parsed.text) {
      if (text.indexOf(parsed.text.toLowerCase()) < 0) show = false;
    }

    el.style.display = show ? '' : 'none';
    if (show) visible++;
  });

  var countEl = document.getElementById(config.countId);
  if (countEl) {
    countEl.textContent = (parsed.text || Object.keys(parsed.filters).length) ? visible + ' / ' + total : '';
  }
}

function parseQuery(query) {
  var filters = {};
  var textParts = [];

  // Match key:value or key:"quoted value" pairs
  var regex = /(\w+):(?:"([^"]+)"|(\S+))/g;
  var match;
  var lastIdx = 0;

  while ((match = regex.exec(query)) !== null) {
    // Collect text before this match
    var before = query.substring(lastIdx, match.index).trim();
    if (before) textParts.push(before);
    lastIdx = match.index + match[0].length;

    var key = match[1];
    var val = match[2] || match[3]; // quoted or unquoted
    filters[key] = val;
  }

  // Remaining text after last match
  var remaining = query.substring(lastIdx).trim();
  if (remaining) textParts.push(remaining);

  return { filters: filters, text: textParts.join(' ') };
}

function getFieldText(el, fieldKey, config) {
  // Try to find the column by data attribute or position
  var cell = el.querySelector('[data-field="' + fieldKey + '"]');
  if (cell) return cell.textContent;

  // Fallback: match by column index from config
  var filterDef = config.filters.find(function(f) { return f.key === fieldKey; });
  if (filterDef && filterDef.col !== undefined) {
    var cells = el.querySelectorAll('td');
    if (cells[filterDef.col]) return cells[filterDef.col].textContent;
  }

  return null; // Fall back to full text search
}
