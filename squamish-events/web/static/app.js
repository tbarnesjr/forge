/* ==========================================================================
   Squamish Events — Application Logic
   ========================================================================== */

(function () {
  'use strict';

  // ---------------------------------------------------------------------------
  // State
  // ---------------------------------------------------------------------------
  const state = {
    events: [],
    total: 0,
    offset: 0,
    limit: 20,
    filters: {
      search: '',
      category: '',
      source: '',
      from: '',
      to: '',
    },
    view: 'list',        // 'list' | 'calendar'
    calYear: new Date().getFullYear(),
    calMonth: new Date().getMonth(),
    calSelectedDate: null, // 'YYYY-MM-DD' or null
    calEvents: {},         // { 'YYYY-MM-DD': count }
    loading: false,
  };

  // ---------------------------------------------------------------------------
  // DOM refs
  // ---------------------------------------------------------------------------
  const $ = (sel) => document.querySelector(sel);
  const $$ = (sel) => document.querySelectorAll(sel);

  const dom = {
    search:          $('#search-input'),
    categoryFilter:  $('#category-filter'),
    sourceFilter:    $('#source-filter'),
    dateFrom:        $('#date-from'),
    dateTo:          $('#date-to'),
    clearFilters:    $('#clear-filters-btn'),
    eventsContainer: $('#events-container'),
    noEvents:        $('#no-events'),
    loadMoreWrap:    $('#load-more-wrap'),
    loadMoreBtn:     $('#load-more-btn'),
    listLoading:     $('#list-loading'),
    eventList:       $('#event-list'),
    calendarView:    $('#calendar-view'),
    calTitle:        $('#cal-title'),
    calGrid:         $('#calendar-grid'),
    calPrev:         $('#cal-prev'),
    calNext:         $('#cal-next'),
    detailDialog:    $('#event-detail-dialog'),
    detailTitle:     $('#detail-title'),
    detailBody:      $('#detail-body'),
    submitDialog:    $('#submit-dialog'),
    openSubmitBtn:   $('#open-submit-btn'),
    closeSubmitBtn:  $('#close-submit-btn'),
    submitForm:      $('#submit-form'),
    submitError:     $('#submit-error'),
    submitSuccess:   $('#submit-success'),
    submitBtn:       $('#submit-btn'),
    subCategory:     $('#sub-category'),
  };

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------
  function formatDate(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return d.toLocaleDateString('en-CA', {
      weekday: 'short', year: 'numeric', month: 'short', day: 'numeric',
    });
  }

  function formatTime(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return d.toLocaleTimeString('en-CA', { hour: 'numeric', minute: '2-digit' });
  }

  function formatDateTime(iso) {
    if (!iso) return '';
    return `${formatDate(iso)} at ${formatTime(iso)}`;
  }

  function toDateKey(iso) {
    return iso ? iso.slice(0, 10) : '';
  }

  function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
  }

  function debounce(fn, ms) {
    let timer;
    return function (...args) {
      clearTimeout(timer);
      timer = setTimeout(() => fn.apply(this, args), ms);
    };
  }

  // ---------------------------------------------------------------------------
  // API
  // ---------------------------------------------------------------------------
  async function apiGet(path, params = {}) {
    const url = new URL(path, window.location.origin);
    Object.entries(params).forEach(([k, v]) => {
      if (v !== '' && v != null) url.searchParams.set(k, v);
    });
    const res = await fetch(url);
    if (!res.ok) throw new Error(`API error: ${res.status}`);
    return res.json();
  }

  async function apiPost(path, body) {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      throw new Error(err.error || `API error: ${res.status}`);
    }
    return res.json();
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------
  async function loadCategories() {
    try {
      const cats = await apiGet('/api/categories');
      const arr = Array.isArray(cats) ? cats : [];
      arr.forEach((cat) => {
        const opt = document.createElement('option');
        opt.value = cat;
        opt.textContent = cat;
        dom.categoryFilter.appendChild(opt);

        const opt2 = opt.cloneNode(true);
        dom.subCategory.appendChild(opt2);
      });
    } catch (e) {
      console.warn('Failed to load categories:', e);
    }
  }

  async function loadSources() {
    try {
      const sources = await apiGet('/api/sources');
      const arr = Array.isArray(sources) ? sources : [];
      arr.forEach((src) => {
        const opt = document.createElement('option');
        opt.value = src.id || src.name || src;
        opt.textContent = src.name || src;
        dom.sourceFilter.appendChild(opt);
      });
    } catch (e) {
      console.warn('Failed to load sources:', e);
    }
  }

  async function fetchEvents(append = false) {
    if (state.loading) return;
    state.loading = true;
    dom.listLoading.hidden = false;

    try {
      const params = {
        search: state.filters.search,
        category: state.filters.category,
        source: state.filters.source,
        from: state.filters.from,
        to: state.filters.to,
        limit: state.limit,
        offset: append ? state.offset : 0,
      };

      const data = await apiGet('/api/events', params);
      const events = Array.isArray(data) ? data : (data.events || []);
      const total = data.total ?? events.length;

      if (append) {
        state.events = state.events.concat(events);
      } else {
        state.events = events;
      }
      state.total = total;
      state.offset = state.events.length;

      renderEventList();
    } catch (e) {
      console.error('Failed to fetch events:', e);
      dom.noEvents.textContent = 'Failed to load events. Please try again.';
      dom.noEvents.hidden = false;
    } finally {
      state.loading = false;
      dom.listLoading.hidden = true;
    }
  }

  async function fetchEventDetail(id) {
    try {
      const event = await apiGet(`/api/events/${id}`);
      showEventDetail(event);
    } catch (e) {
      console.error('Failed to load event:', e);
    }
  }

  // ---------------------------------------------------------------------------
  // Render: Event List
  // ---------------------------------------------------------------------------
  function renderEventList() {
    const events = state.events;
    dom.eventsContainer.innerHTML = '';

    if (events.length === 0) {
      dom.noEvents.textContent = 'No events found.';
      dom.noEvents.hidden = false;
      dom.loadMoreWrap.hidden = true;
      return;
    }

    dom.noEvents.hidden = true;

    events.forEach((evt) => {
      const card = document.createElement('article');
      card.className = 'event-card';
      card.dataset.id = evt.id;
      card.setAttribute('role', 'button');
      card.setAttribute('tabindex', '0');

      card.innerHTML = `
        <h3>${escapeHtml(evt.title || 'Untitled Event')}</h3>
        <div class="event-meta">
          <span><span class="meta-icon">📅</span> ${formatDate(evt.start_time)}</span>
          ${evt.start_time ? `<span><span class="meta-icon">🕐</span> ${formatTime(evt.start_time)}</span>` : ''}
          ${evt.location ? `<span><span class="meta-icon">📍</span> ${escapeHtml(evt.location)}</span>` : ''}
        </div>
        <div class="event-tags">
          ${evt.category ? `<span class="event-category">${escapeHtml(evt.category)}</span>` : ''}
          ${evt.source_name ? `<span class="event-source">${escapeHtml(evt.source_name)}</span>` : ''}
        </div>
      `;

      card.addEventListener('click', () => fetchEventDetail(evt.id));
      card.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          fetchEventDetail(evt.id);
        }
      });

      dom.eventsContainer.appendChild(card);
    });

    // Load more
    const hasMore = state.offset < state.total;
    dom.loadMoreWrap.hidden = !hasMore;
  }

  // ---------------------------------------------------------------------------
  // Render: Event Detail
  // ---------------------------------------------------------------------------
  function showEventDetail(evt) {
    dom.detailTitle.textContent = evt.title || 'Event Details';

    let html = '<dl>';

    if (evt.start_time) {
      html += `<div class="detail-field"><dt>When</dt><dd>${formatDateTime(evt.start_time)}`;
      if (evt.end_time) html += ` — ${formatDateTime(evt.end_time)}`;
      html += `</dd></div>`;
    }

    if (evt.location) {
      html += `<div class="detail-field"><dt>Where</dt><dd>${escapeHtml(evt.location)}</dd></div>`;
    }

    if (evt.category) {
      html += `<div class="detail-field"><dt>Category</dt><dd><span class="event-category">${escapeHtml(evt.category)}</span></dd></div>`;
    }

    if (evt.source_name) {
      html += `<div class="detail-field"><dt>Source</dt><dd><span class="event-source">${escapeHtml(evt.source_name)}</span></dd></div>`;
    }

    if (evt.url) {
      html += `<div class="detail-field"><dt>Link</dt><dd><a href="${escapeHtml(evt.url)}" target="_blank" rel="noopener">${escapeHtml(evt.url)}</a></dd></div>`;
    }

    html += '</dl>';

    if (evt.description) {
      html += `<div class="detail-description">${escapeHtml(evt.description)}</div>`;
    }

    dom.detailBody.innerHTML = html;
    dom.detailDialog.showModal();
  }

  // ---------------------------------------------------------------------------
  // Calendar
  // ---------------------------------------------------------------------------
  async function loadCalendarEvents() {
    const year = state.calYear;
    const month = state.calMonth;
    const from = `${year}-${String(month + 1).padStart(2, '0')}-01`;

    // Last day of month
    const lastDay = new Date(year, month + 1, 0).getDate();
    const to = `${year}-${String(month + 1).padStart(2, '0')}-${String(lastDay).padStart(2, '0')}`;

    try {
      const params = {
        from,
        to,
        limit: 500,
        category: state.filters.category,
        source: state.filters.source,
        search: state.filters.search,
      };

      const data = await apiGet('/api/events', params);
      const events = Array.isArray(data) ? data : (data.events || []);

      // Count events per day
      const counts = {};
      events.forEach((evt) => {
        const key = toDateKey(evt.start_time);
        if (key) counts[key] = (counts[key] || 0) + 1;
      });
      state.calEvents = counts;
    } catch (e) {
      console.error('Failed to load calendar events:', e);
      state.calEvents = {};
    }

    renderCalendar();
  }

  function renderCalendar() {
    const year = state.calYear;
    const month = state.calMonth;

    const monthNames = [
      'January', 'February', 'March', 'April', 'May', 'June',
      'July', 'August', 'September', 'October', 'November', 'December',
    ];
    dom.calTitle.textContent = `${monthNames[month]} ${year}`;

    const dayHeaders = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    let html = dayHeaders.map((d) => `<div class="cal-header">${d}</div>`).join('');

    const firstDay = new Date(year, month, 1).getDay();
    const daysInMonth = new Date(year, month + 1, 0).getDate();

    const today = new Date();
    const todayKey = `${today.getFullYear()}-${String(today.getMonth() + 1).padStart(2, '0')}-${String(today.getDate()).padStart(2, '0')}`;

    // Empty cells before first day
    for (let i = 0; i < firstDay; i++) {
      html += '<div class="cal-day empty"></div>';
    }

    for (let d = 1; d <= daysInMonth; d++) {
      const dateKey = `${year}-${String(month + 1).padStart(2, '0')}-${String(d).padStart(2, '0')}`;
      const count = state.calEvents[dateKey] || 0;
      const isToday = dateKey === todayKey;
      const isSelected = dateKey === state.calSelectedDate;

      let classes = 'cal-day';
      if (count > 0) classes += ' has-events';
      if (isToday) classes += ' today';
      if (isSelected) classes += ' selected';

      let dotsHtml = '';
      if (count > 0 && count <= 3) {
        dotsHtml = '<div class="cal-dots">' + '<span class="cal-dot"></span>'.repeat(count) + '</div>';
      } else if (count > 3) {
        dotsHtml = `<span class="cal-count">${count}</span>`;
      }

      html += `<div class="${classes}" data-date="${dateKey}">
        <span class="cal-day-num">${d}</span>
        ${dotsHtml}
      </div>`;
    }

    dom.calGrid.innerHTML = html;

    // Click handlers for days with events
    dom.calGrid.querySelectorAll('.cal-day.has-events').forEach((el) => {
      el.addEventListener('click', () => {
        const date = el.dataset.date;
        if (state.calSelectedDate === date) {
          // Deselect
          state.calSelectedDate = null;
          state.filters.from = '';
          state.filters.to = '';
        } else {
          state.calSelectedDate = date;
          state.filters.from = date;
          state.filters.to = date;
        }
        dom.dateFrom.value = state.filters.from;
        dom.dateTo.value = state.filters.to;
        renderCalendar();
        fetchEvents();
      });
    });
  }

  // ---------------------------------------------------------------------------
  // View switching
  // ---------------------------------------------------------------------------
  function setView(view) {
    state.view = view;

    $$('.view-btn').forEach((btn) => {
      btn.classList.toggle('active', btn.dataset.view === view);
    });

    if (view === 'list') {
      dom.eventList.hidden = false;
      dom.calendarView.hidden = true;
    } else {
      dom.eventList.hidden = true;
      dom.calendarView.hidden = false;
      loadCalendarEvents();
    }
  }

  // ---------------------------------------------------------------------------
  // Filters
  // ---------------------------------------------------------------------------
  function applyFilters() {
    state.filters.search = dom.search.value.trim();
    state.filters.category = dom.categoryFilter.value;
    state.filters.source = dom.sourceFilter.value;
    state.filters.from = dom.dateFrom.value;
    state.filters.to = dom.dateTo.value;
    state.offset = 0;
    state.calSelectedDate = null;

    fetchEvents();
    if (state.view === 'calendar') {
      loadCalendarEvents();
    }
  }

  function clearFilters() {
    dom.search.value = '';
    dom.categoryFilter.value = '';
    dom.sourceFilter.value = '';
    dom.dateFrom.value = '';
    dom.dateTo.value = '';
    state.calSelectedDate = null;
    applyFilters();
  }

  // ---------------------------------------------------------------------------
  // Submit Event
  // ---------------------------------------------------------------------------
  async function handleSubmit(e) {
    e.preventDefault();
    dom.submitError.hidden = true;
    dom.submitSuccess.hidden = true;

    const form = dom.submitForm;
    const data = {
      title: form.title.value.trim(),
      description: form.description.value.trim(),
      location: form.location.value.trim(),
      category: form.category.value,
      start_time: form.start_time.value ? new Date(form.start_time.value).toISOString() : '',
      end_time: form.end_time.value ? new Date(form.end_time.value).toISOString() : '',
      url: form.url.value.trim(),
      contact_email: form.contact_email.value.trim(),
    };

    // Basic validation
    if (!data.title || !data.description || !data.location || !data.category || !data.start_time || !data.contact_email) {
      dom.submitError.textContent = 'Please fill in all required fields.';
      dom.submitError.hidden = false;
      return;
    }

    dom.submitBtn.disabled = true;
    dom.submitBtn.setAttribute('aria-busy', 'true');

    try {
      await apiPost('/api/submissions', data);
      dom.submitSuccess.hidden = false;
      form.reset();
      setTimeout(() => {
        dom.submitDialog.close();
        dom.submitSuccess.hidden = true;
      }, 2000);
    } catch (err) {
      dom.submitError.textContent = err.message || 'Submission failed. Please try again.';
      dom.submitError.hidden = false;
    } finally {
      dom.submitBtn.disabled = false;
      dom.submitBtn.removeAttribute('aria-busy');
    }
  }

  // ---------------------------------------------------------------------------
  // Event Listeners
  // ---------------------------------------------------------------------------
  function bindEvents() {
    // Search
    dom.search.addEventListener('input', debounce(applyFilters, 300));

    // Filter dropdowns / dates
    dom.categoryFilter.addEventListener('change', applyFilters);
    dom.sourceFilter.addEventListener('change', applyFilters);
    dom.dateFrom.addEventListener('change', applyFilters);
    dom.dateTo.addEventListener('change', applyFilters);
    dom.clearFilters.addEventListener('click', clearFilters);

    // Load more
    dom.loadMoreBtn.addEventListener('click', () => fetchEvents(true));

    // View toggle
    $$('.view-btn').forEach((btn) => {
      btn.addEventListener('click', () => setView(btn.dataset.view));
    });

    // Calendar nav
    dom.calPrev.addEventListener('click', () => {
      state.calMonth--;
      if (state.calMonth < 0) {
        state.calMonth = 11;
        state.calYear--;
      }
      state.calSelectedDate = null;
      loadCalendarEvents();
    });

    dom.calNext.addEventListener('click', () => {
      state.calMonth++;
      if (state.calMonth > 11) {
        state.calMonth = 0;
        state.calYear++;
      }
      state.calSelectedDate = null;
      loadCalendarEvents();
    });

    // Detail dialog close
    dom.detailDialog.querySelectorAll('[data-close-detail]').forEach((btn) => {
      btn.addEventListener('click', () => dom.detailDialog.close());
    });
    dom.detailDialog.addEventListener('click', (e) => {
      if (e.target === dom.detailDialog) dom.detailDialog.close();
    });

    // Submit dialog
    dom.openSubmitBtn.addEventListener('click', () => {
      dom.submitError.hidden = true;
      dom.submitSuccess.hidden = true;
      dom.submitDialog.showModal();
    });
    dom.closeSubmitBtn.addEventListener('click', () => dom.submitDialog.close());
    dom.submitDialog.addEventListener('click', (e) => {
      if (e.target === dom.submitDialog) dom.submitDialog.close();
    });
    dom.submitForm.addEventListener('submit', handleSubmit);
  }

  // ---------------------------------------------------------------------------
  // Init
  // ---------------------------------------------------------------------------
  async function init() {
    bindEvents();
    await Promise.all([loadCategories(), loadSources()]);
    fetchEvents();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
