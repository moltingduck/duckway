/**
 * Duckway Smart Search + Pagination
 *
 * Supports:
 * - Plain text search (matches any column)
 * - Filter syntax: field:value (e.g., service:openai status:active)
 * - Multiple filters: service:openai client:laptop
 * - Quoted values: name:"my key"
 * - Autocomplete dropdown with suggestions
 * - Pagination (default 15 per page, search across ALL data)
 */

function initSmartSearch(config) {
  // config: { inputId, targetId, type: 'table'|'cards',
  //           filters: [{key, label, values, col}], countId, perPage }

  var input = document.getElementById(config.inputId);
  if (!input) return;

  config.perPage = config.perPage || 15;
  config.currentPage = 1;

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

  // Pagination container
  var pager = document.createElement('div');
  pager.className = 'pagination';
  pager.id = config.inputId + '-pager';
  var target = document.getElementById(config.targetId);
  if (target && target.parentNode) {
    target.parentNode.insertBefore(pager, target.nextSibling);
  }

  input.addEventListener('input', function() {
    config.currentPage = 1; // Reset to page 1 on new search
    applyFilter(config, input.value);
    showSuggestions(config, dropdown, input.value, input);
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

  // Initial render with pagination
  applyFilter(config, '');
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
  config.currentPage = 1;
  applyFilter(config, insert);
}

function applyFilter(config, query) {
  var parsed = parseQuery(query);
  var target = document.getElementById(config.targetId);
  if (!target) return;

  var elements = config.type === 'cards'
    ? Array.from(target.querySelectorAll('[id^="client-card-"]'))
    : Array.from(target.querySelectorAll('tr'));

  // Filter all elements
  var matched = [];
  elements.forEach(function(el) {
    var text = el.textContent.toLowerCase();
    var show = true;

    for (var key in parsed.filters) {
      var val = parsed.filters[key].toLowerCase();
      var colText = getFieldText(el, key, config);
      if (colText !== null) {
        if (colText.toLowerCase().indexOf(val) < 0) { show = false; break; }
      } else {
        if (text.indexOf(val) < 0) { show = false; break; }
      }
    }

    if (show && parsed.text) {
      if (text.indexOf(parsed.text.toLowerCase()) < 0) show = false;
    }

    if (show) matched.push(el);
  });

  // Paginate
  var total = matched.length;
  var totalPages = Math.ceil(total / config.perPage) || 1;
  if (config.currentPage > totalPages) config.currentPage = totalPages;
  var start = (config.currentPage - 1) * config.perPage;
  var end = start + config.perPage;

  // Hide all, show only current page
  elements.forEach(function(el) { el.style.display = 'none'; });
  matched.slice(start, end).forEach(function(el) { el.style.display = ''; });

  // Update count
  var countEl = document.getElementById(config.countId);
  if (countEl) {
    var showing = Math.min(end, total) - start;
    if (total === elements.length && totalPages === 1) {
      countEl.textContent = total + ' items';
    } else {
      countEl.textContent = (start + 1) + '–' + (start + showing) + ' of ' + total + (total !== elements.length ? ' (filtered from ' + elements.length + ')' : '');
    }
  }

  // Render pagination controls
  renderPager(config, totalPages, total, query);
}

function renderPager(config, totalPages, total, query) {
  var pager = document.getElementById(config.inputId + '-pager');
  if (!pager) return;

  if (totalPages <= 1) {
    pager.innerHTML = '';
    return;
  }

  var html = '';
  // Prev
  if (config.currentPage > 1) {
    html += '<button class="btn btn-sm" onclick="goPage(\'' + config.inputId + '\',' + (config.currentPage - 1) + ')">← Prev</button> ';
  }

  // Page numbers
  for (var i = 1; i <= totalPages; i++) {
    if (totalPages > 7 && i > 3 && i < totalPages - 2 && Math.abs(i - config.currentPage) > 1) {
      if (html.slice(-3) !== '...') html += '<span class="text-muted text-sm"> ... </span>';
      continue;
    }
    if (i === config.currentPage) {
      html += '<button class="btn btn-sm btn-primary" disabled>' + i + '</button> ';
    } else {
      html += '<button class="btn btn-sm" onclick="goPage(\'' + config.inputId + '\',' + i + ')">' + i + '</button> ';
    }
  }

  // Next
  if (config.currentPage < totalPages) {
    html += '<button class="btn btn-sm" onclick="goPage(\'' + config.inputId + '\',' + (config.currentPage + 1) + ')">Next →</button>';
  }

  pager.innerHTML = html;
}

// Global page navigation (called from rendered buttons)
var _searchConfigs = {};
function goPage(inputId, page) {
  var cfg = _searchConfigs[inputId];
  if (!cfg) return;
  cfg.currentPage = page;
  var input = document.getElementById(inputId);
  applyFilter(cfg, input ? input.value : '');
}

// Wrap initSmartSearch to register config globally
var _origInit = initSmartSearch;
initSmartSearch = function(config) {
  _searchConfigs[config.inputId] = config;
  _origInit(config);
};

function parseQuery(query) {
  var filters = {};
  var textParts = [];

  var regex = /(\w+):(?:"([^"]+)"|(\S+))/g;
  var match;
  var lastIdx = 0;

  while ((match = regex.exec(query)) !== null) {
    var before = query.substring(lastIdx, match.index).trim();
    if (before) textParts.push(before);
    lastIdx = match.index + match[0].length;
    filters[match[1]] = match[2] || match[3];
  }

  var remaining = query.substring(lastIdx).trim();
  if (remaining) textParts.push(remaining);

  return { filters: filters, text: textParts.join(' ') };
}

function getFieldText(el, fieldKey, config) {
  var cell = el.querySelector('[data-field="' + fieldKey + '"]');
  if (cell) return cell.textContent;

  var filterDef = config.filters.find(function(f) { return f.key === fieldKey; });
  if (filterDef && filterDef.col !== undefined) {
    var cells = el.querySelectorAll('td');
    if (cells[filterDef.col]) return cells[filterDef.col].textContent;
  }

  return null;
}
