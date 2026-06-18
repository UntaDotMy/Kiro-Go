        let password = localStorage.getItem('admin_password') || '';
        const baseUrl = location.origin;
        let accountsData = [];
        let selectedAccounts = new Set();
        let filterKeyword = '';
        let filterStatus = 'all';

        function uiToast(message, kind) {
            const host = document.getElementById('toastHost');
            if (!host) { return; }
            const el = document.createElement('div');
            el.className = 'toast' + (kind === 'success' ? ' toast-success' : kind === 'error' ? ' toast-error' : '');
            const okIcon = '<svg class="toast-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M20 6L9 17l-5-5"/></svg>';
            const errIcon = '<svg class="toast-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="12" cy="12" r="10"/><path d="M12 8v5M12 16h.01"/></svg>';
            const icon = kind === 'success' ? okIcon : kind === 'error' ? errIcon : '';
            const msg = document.createElement('span');
            msg.className = 'toast-msg';
            msg.textContent = message == null ? '' : String(message);
            el.innerHTML = icon;
            el.appendChild(msg);
            const close = document.createElement('button');
            close.className = 'toast-close';
            close.setAttribute('aria-label', 'Close');
            close.innerHTML = '&times;';
            const dismiss = () => {
                el.classList.add('is-leaving');
                el.addEventListener('animationend', () => el.remove(), { once: true });
            };
            close.onclick = dismiss;
            el.appendChild(close);
            host.appendChild(el);
            setTimeout(dismiss, kind === 'error' ? 6000 : 3800);
        }

        window.alert = function (message) {
            const text = message == null ? '' : String(message);
            const lower = text.toLowerCase();
            const kind = /fail|error|invalid|required|missing|not detected|unknown|无效|失败|错误|必填|缺少/.test(lower) || /失败|错误|无效|必填|缺少/.test(text) ? 'error' : 'success';
            uiToast(text, kind);
        };

        function uiConfirm(message, opts) {
            opts = opts || {};
            return new Promise((resolve) => {
                const modal = document.getElementById('confirmModal');
                const titleEl = document.getElementById('confirmModalTitle');
                const msgEl = document.getElementById('confirmModalMsg');
                const fieldWrap = document.getElementById('confirmModalField');
                const okBtn = document.getElementById('confirmModalOk');
                const cancelBtn = document.getElementById('confirmModalCancel');
                const tt = (typeof t === 'function') ? t : (k => k);
                titleEl.textContent = opts.title || tt('common.confirm') || 'Confirm';
                msgEl.textContent = message == null ? '' : String(message);
                okBtn.textContent = opts.okText || tt('common.confirm') || 'OK';
                cancelBtn.textContent = opts.cancelText || tt('common.cancel') || 'Cancel';
                okBtn.className = 'btn btn-sm ' + (opts.danger ? 'btn-danger' : 'btn-primary');
                let input = null;
                if (opts.prompt) {
                    fieldWrap.classList.remove('hidden');
                    fieldWrap.innerHTML = '';
                    input = document.createElement('input');
                    input.type = 'text';
                    input.value = opts.defaultValue || '';
                    if (opts.placeholder) { input.placeholder = opts.placeholder; }
                    fieldWrap.appendChild(input);
                } else {
                    fieldWrap.classList.add('hidden');
                    fieldWrap.innerHTML = '';
                }
                const cleanup = (result) => {
                    modal.classList.remove('active');
                    okBtn.onclick = null;
                    cancelBtn.onclick = null;
                    modal.onclick = null;
                    document.removeEventListener('keydown', onKey);
                    resolve(result);
                };
                const onKey = (e) => {
                    if (e.key === 'Escape') { cleanup(opts.prompt ? null : false); }
                    else if (e.key === 'Enter' && !opts.prompt) { cleanup(true); }
                };
                okBtn.onclick = () => cleanup(opts.prompt ? (input ? input.value : '') : true);
                cancelBtn.onclick = () => cleanup(opts.prompt ? null : false);
                modal.onclick = (e) => { if (e.target === modal) { cleanup(opts.prompt ? null : false); } };
                document.addEventListener('keydown', onKey);
                modal.classList.add('active');
                if (input) { setTimeout(() => input.focus(), 30); }
                else { setTimeout(() => okBtn.focus(), 30); }
            });
        }

        function uiPrompt(message, defaultValue, opts) {
            opts = opts || {};
            return uiConfirm(message, Object.assign({ prompt: true, defaultValue: defaultValue || '' }, opts));
        }

        // 隐私模式状态管理
        let privacyModeEnabled = true;

        // 初始化隐私模式
        function initPrivacyMode() {
            try {
                const saved = localStorage.getItem('privacyMode');
                privacyModeEnabled = saved === null ? true : saved === 'true';
                const toggle = document.getElementById('privacyModeToggle');
                if (toggle) toggle.checked = privacyModeEnabled;
            } catch (e) {
                console.warn('localStorage not available:', e);
            }
        }

        // 切换隐私模式
        function togglePrivacyMode() {
            const toggle = document.getElementById('privacyModeToggle');
            privacyModeEnabled = toggle.checked;
            try {
                localStorage.setItem('privacyMode', privacyModeEnabled);
            } catch (e) {
                console.warn('localStorage not available:', e);
            }
            renderAccounts();
        }

        // 邮箱脱敏函数
        function maskEmail(email) {
            if (!privacyModeEnabled || !email || email.indexOf('@') === -1) {
                return email;
            }

            const [localPart, domain] = email.split('@');

            // 本地部分脱敏：保留前 2 个字符
            const maskedLocal = localPart.length <= 2
                ? localPart
                : localPart.substring(0, 2) + '***';

            // 域名部分脱敏
            const domainParts = domain.split('.');
            if (domainParts.length >= 2) {
                const tld = domainParts[domainParts.length - 1]; // 顶级域名
                const sld = domainParts[domainParts.length - 2]; // 二级域名
                const maskedSld = sld.length <= 2
                    ? sld
                    : sld.substring(0, 2) + '***';

                // 子域名脱敏
                const subdomains = domainParts.slice(0, -2).map(sub =>
                    sub.length <= 2 ? sub : sub.substring(0, 2) + '***'
                );

                return maskedLocal + '@' + [...subdomains, maskedSld, tld].join('.');
            }

            return maskedLocal + '@' + domain;
        }

        // 统一获取显示用邮箱
        function getDisplayEmail(email, accountId) {
            const raw = email || (accountId ? accountId.substring(0, 12) + '...' : '-');
            return maskEmail(raw);
        }

        document.addEventListener('DOMContentLoaded', function () {
            updateLangButtons();
            applyTranslations();
            initPrivacyMode();
            if (password) tryAutoLogin();
            document.getElementById('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });
            document.querySelectorAll('.tab').forEach(tab => { tab.onclick = () => switchTab(tab.dataset.tab); });
        });
        async function tryAutoLogin() {
            // 72h 过期检查
            const loginTime = parseInt(localStorage.getItem('admin_login_time') || '0');
            if (loginTime && Date.now() - loginTime > 72 * 3600 * 1000) {
                localStorage.removeItem('admin_password');
                localStorage.removeItem('admin_login_time');
                password = '';
                return;
            }
            // Optimistic restore: a saved, non-expired password means the operator
            // was logged in, so show the dashboard IMMEDIATELY instead of flashing
            // the login page during the async verify. We then verify in the
            // background and only fall back to login if the password is actually
            // rejected (revoked / changed). This removes the login flash on every
            // refresh while still re-authenticating each session.
            showMain();
            loadData();
            try {
                const res = await fetch('/admin/api/status', { headers: { 'X-Admin-Password': password } });
                if (!res.ok) {
                    // Password no longer valid — drop it and return to login.
                    localStorage.removeItem('admin_password');
                    localStorage.removeItem('admin_login_time');
                    password = '';
                    showLogin();
                }
            } catch (e) {
                // Network blip — keep the optimistic view; the WS + poll fallback
                // will recover when connectivity returns. Don't bounce to login on
                // a transient error.
            }
        }
        // showLogin returns to the login view. It MUST clear the pre-authed class
        // on <html> as well as toggling the element .hidden classes: the
        // `html.pre-authed #mainPage { display:flex !important }` rule (set
        // pre-paint to kill the login flash) otherwise overrides the element
        // classes and would keep a revoked session stuck on the dashboard.
        function showLogin() {
            try { document.documentElement.classList.remove('pre-authed'); } catch (e) { }
            document.getElementById('mainPage').classList.add('hidden');
            document.getElementById('loginPage').classList.remove('hidden');
        }
        async function login() {
            password = document.getElementById('pwdField').value;
            try {
                const res = await fetch('/admin/api/status', { headers: { 'X-Admin-Password': password } });
                if (res.ok) {
                    localStorage.setItem('admin_password', password);
                    localStorage.setItem('admin_login_time', Date.now().toString());
                    showMain(); loadData();
                } else {
                    document.getElementById('loginError').textContent = t('login.error');
                    document.getElementById('loginError').classList.remove('hidden');
                }
            } catch (e) {
                document.getElementById('loginError').textContent = t('login.connectError');
                document.getElementById('loginError').classList.remove('hidden');
            }
        }
        function logout() {
            localStorage.removeItem('admin_password');
            localStorage.removeItem('admin_login_time');
            location.reload();
        }
        function showMain() {
            document.getElementById('loginPage').classList.add('hidden');
            document.getElementById('mainPage').classList.remove('hidden');
            // Open the realtime status WebSocket on every successful entry
            // into the main view — covers both fresh logins and the
            // localStorage auto-login path. startDashboardWS is idempotent
            // (it checks dashboardWS before opening another connection).
            startDashboardWS();
        }
        async function loadData() {
            await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion()]);
            // Re-run stats so credits-total / remaining can pick up the
            // accounts that finished loading concurrently.
            loadStats();
            document.getElementById('claudeEndpoint').textContent = baseUrl + '/v1/messages';
            document.getElementById('openaiEndpoint').textContent = baseUrl + '/v1/chat/completions';
            document.getElementById('responsesEndpoint').textContent = baseUrl + '/v1/responses';
            document.getElementById('modelsEndpoint').textContent = baseUrl + '/v1/models';
            document.getElementById('statsEndpoint').textContent = baseUrl + '/v1/stats';
            const ksEl = document.getElementById('keystatusEndpoint');
            if (ksEl) ksEl.textContent = baseUrl + '/v1/key-status';
            renderApiCode();
            // 自动检查更新
            setTimeout(() => checkUpdate(false), 2000);
        }
        async function loadStats() {
            const res = await fetch('/admin/api/status', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            // Delegate to the shared renderer so push and poll paths agree
            // on every tile. renderStatusFromObject is defined further down
            // alongside the WS plumbing; calling order works because both
            // exist by the time loadStats fires (DOM-ready handler runs
            // after script parse).
            try { renderStatusFromObject(d); return; } catch (e) {}
            // Fallback (shouldn't normally hit) — original inline render to
            // keep the dashboard alive even if the renderer throws.
            document.getElementById('statAccounts').textContent = d.accounts || 0;
            document.getElementById('statAvailable').textContent = (d.available !== undefined ? d.available : (d.accounts || 0));
            document.getElementById('statRequests').textContent = d.totalRequests || 0;
            document.getElementById('statSuccess').textContent = d.successRequests || 0;
            document.getElementById('statFailed').textContent = d.failedRequests || 0;
            document.getElementById('statTokens').textContent = formatNum(d.totalTokens || 0);
        }
        function formatUptime(secs) {
            if (!secs || secs <= 0) return '-';
            const d = Math.floor(secs / 86400);
            const h = Math.floor((secs % 86400) / 3600);
            const m = Math.floor((secs % 3600) / 60);
            if (d > 0) return d + 'd ' + h + 'h';
            if (h > 0) return h + 'h ' + m + 'm';
            return m + 'm';
        }
        async function loadAccounts() {
            const res = await fetch('/admin/api/accounts', { headers: { 'X-Admin-Password': password } });
            accountsData = await res.json();
            renderAccounts();
        }
        function getFilteredAccounts() {
            return accountsData.filter(a => {
                // The Accounts tab is Kiro-only. Non-Kiro provider accounts
                // (codex/qoder/openai/groq/custom...) live under the Providers tab,
                // which has its own quota/model semantics. A Backend-less account
                // is a legacy Kiro account, so treat empty/"kiro" as Kiro.
                const backend = (a.backend || 'kiro').toLowerCase();
                if (backend !== 'kiro') return false;
                if (filterStatus === 'enabled' && !a.enabled) return false;
                if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
                if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
                if (filterKeyword) {
                    const kw = filterKeyword.toLowerCase();
                    const email = (a.email || '').toLowerCase();
                    if (!email.includes(kw)) return false;
                }
                return true;
            });
        }
        // providerAccounts returns the non-Kiro accounts, grouped for the
        // Providers tab. Each is keyed by its backend (the provider id).
        function providerAccounts() {
            return accountsData.filter(a => (a.backend || 'kiro').toLowerCase() !== 'kiro');
        }
        function onFilterChange() {
            filterKeyword = document.getElementById('filterSearch').value;
            filterStatus = document.getElementById('filterStatusSelect').value;
            renderAccounts();
        }
        function toggleSelectAll(checked) {
            const filtered = getFilteredAccounts();
            if (checked) {
                filtered.forEach(a => selectedAccounts.add(a.id));
            } else {
                selectedAccounts.clear();
            }
            renderAccounts();
            updateBatchBar();
        }
        function toggleSelectAccount(id) {
            if (selectedAccounts.has(id)) {
                selectedAccounts.delete(id);
            } else {
                selectedAccounts.add(id);
            }
            updateBatchBar();
            const cb = document.getElementById('selectAllCheckbox');
            if (cb) {
                const filtered = getFilteredAccounts();
                cb.checked = filtered.length > 0 && filtered.every(a => selectedAccounts.has(a.id));
            }
        }
        function updateBatchBar() {
            const bar = document.getElementById('batchBar');
            const count = selectedAccounts.size;
            if (count > 0) {
                bar.style.display = 'flex';
                document.getElementById('batchCount').textContent = t('batch.selected', count);
            } else {
                bar.style.display = 'none';
            }
        }
        async function batchAction(action) {
            const ids = Array.from(selectedAccounts);
            if (ids.length === 0) return;
            // Map each action to its confirm-message i18n key. Overage on/off
            // and delete have their own copy; enable/disable/refresh keep the
            // existing capitalized-suffix convention.
            const confirmKeyMap = {
                'overage-on': 'batch.confirmOverageOn',
                'overage-off': 'batch.confirmOverageOff',
                'delete': 'batch.confirmDelete'
            };
            const confirmKey = confirmKeyMap[action] || ('batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1));
            const danger = (action === 'delete');
            if (!await uiConfirm(t(confirmKey, ids.length), { danger })) return;
            try {
                const res = await fetch('/admin/api/accounts/batch', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ ids, action })
                });
                const d = await res.json();
                if (action === 'refresh' && d.success) {
                    alert(t('batch.refreshResult', d.refreshed, d.failed));
                } else if (action === 'delete') {
                    alert(t('batch.deleteResult', d.deleted || 0, d.failed || 0));
                }
                selectedAccounts.clear();
                updateBatchBar();
                loadAccounts();
                loadStats();
            } catch (e) {
                alert(t('common.failed'));
            }
        }
        async function batchRefreshModels() {
            const ids = Array.from(selectedAccounts);
            if (ids.length === 0) return;
            if (!await uiConfirm(t('batch.confirmRefreshModels', ids.length))) return;
            let success = 0, fail = 0;
            for (const id of ids) {
                try {
                    const res = await fetch('/admin/api/accounts/' + id + '/models/refresh', {
                        method: 'POST', headers: { 'X-Admin-Password': password }
                    });
                    const d = await res.json();
                    if (d.success) success++; else fail++;
                } catch { fail++; }
            }
            alert(t('batch.refreshModelsResult', success, fail));
            selectedAccounts.clear();
            updateBatchBar();
            loadAccounts();
        }
        async function refreshAllModels() {
            if (!await uiConfirm(t('models.confirmRefreshAll'))) return;
            try {
                const res = await fetch('/admin/api/accounts/models/refresh', {
                    method: 'POST', headers: { 'X-Admin-Password': password }
                });
                const d = await res.json();
                alert(t('models.refreshAllDone', d.refreshed || 0));
            } catch (e) {
                alert(t('common.failed'));
            }
        }
        async function refreshAccountModels(id) {
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/models/refresh', {
                    method: 'POST', headers: { 'X-Admin-Password': password }
                });
                const d = await res.json();
                // For CodeBuddy accounts, also sync upstream quota so the Quota column
                // populates (and surface any quota error, e.g. an expired OAuth token).
                const acct = (accountsData || []).find(x => x.id === id);
                const backend = acct ? (acct.backend || '').toLowerCase() : '';
                let quotaMsg = '';
                if (backend === 'codebuddy' || backend === 'codebuddy-ai') {
                    try {
                        const qres = await fetch('/admin/api/codebuddy/quota/' + id, {
                            method: 'POST', headers: { 'X-Admin-Password': password }
                        });
                        const qd = await qres.json();
                        if (qres.ok && qd.status === 'ok') {
                            quotaMsg = '\nQuota: ' + (qd.used || 0).toFixed(0) + ' / ' + (qd.total || 0).toFixed(0) + ' (' + (qd.plan || '') + ')';
                        } else {
                            quotaMsg = '\nQuota sync failed: ' + (qd.error || qd.warning || 'unknown');
                        }
                    } catch (qe) {
                        quotaMsg = '\nQuota sync error';
                    }
                }
                if (d.success) {
                    // Re-pull accounts so modelCount updates, then re-render whichever
                    // tab is showing this account (providers or accounts).
                    await loadAccounts();
                    if (activeDashboardTab() === 'providers') renderProviders();
                    alert(t('detail.refreshModelCache') + ' OK (' + (d.count || 0) + ' models)' + quotaMsg);
                } else {
                    alert(t('common.failed') + ': ' + (d.error || '') + quotaMsg);
                }
            } catch (e) {
                alert(t('common.failed'));
            }
        }
        function getSubBadge(type) {
            const subType = (type || '').toUpperCase();
            if (subType.includes('POWER')) return '<span class="badge badge-power">POWER</span>';
            if (subType.includes('PRO_PLUS') || subType.includes('PROPLUS')) return '<span class="badge badge-proplus">PRO+</span>';
            if (subType.includes('PRO')) return '<span class="badge badge-pro">PRO</span>';
            return '<span class="badge badge-free">FREE</span>';
        }

        function getTrialBadge(account) {
            if (account.trialStatus === 'ACTIVE' && account.trialUsageLimit > 0) {
                return '<span class="badge badge-trial">' + t('accounts.trial') + '</span>';
            }
            return '';
        }

        function formatTrialExpiry(timestamp) {
            if (!timestamp) return '';
            const date = new Date(timestamp * 1000);
            const now = new Date();
            const diffDays = Math.ceil((date - now) / (1000 * 60 * 60 * 24));
            if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
            if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
            if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
            return '';
        }

        function formatAuthMethod(method) {
            if (!method) return '-';
            if (method === 'idc') return 'Enterprise';
            if (method === 'social') return 'Social';
            return method;
        }
        function getStatusBadge(a) {
            let badges = [];

            // 检查是否为封禁状态
            const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';

            if (isBanned) {
                // 封禁账号：显示"封禁 + 禁用"
                if (a.banStatus === 'BANNED') {
                    badges.push('<span class="badge badge-banned">' + t('accounts.banned') + '</span>');
                } else if (a.banStatus === 'SUSPENDED') {
                    badges.push('<span class="badge badge-suspended">' + t('accounts.suspended') + '</span>');
                }
                // 封禁账号必定显示禁用状态
                badges.push('<span class="badge badge-warning">' + t('accounts.disabled') + '</span>');
            } else {
                // 正常账号：显示"正常 + 启用/禁用"

                // 检查Token状态
                if (!a.hasToken) {
                    badges.push('<span class="badge badge-error">' + t('accounts.noToken') + '</span>');
                } else if (a.expiresAt && a.expiresAt < Date.now() / 1000) {
                    badges.push('<span class="badge badge-warning">' + t('accounts.expired') + '</span>');
                } else {
                    // 有效Token的正常账号显示"正常"
                    badges.push('<span class="badge badge-success">' + t('accounts.normal') + '</span>');
                }

                // 显示启用/禁用状态
                if (a.enabled) {
                    badges.push('<span class="badge badge-info">' + t('accounts.enabled') + '</span>');
                } else {
                    badges.push('<span class="badge badge-warning">' + t('accounts.disabled') + '</span>');
                }
            }

            return badges.join('');
        }
        function formatTokenExpiry(ts) {
            if (!ts) return '-';
            const diff = ts - Date.now() / 1000;
            if (diff <= 0) return t('time.expired');
            if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
            if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
            return Math.floor(diff / 86400) + t('time.days');
        }
        async function refreshAccount(id) {
            const card = event.target.closest('.account-card');
            if (card) card.classList.add('loading');
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/refresh', { method: 'POST', headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                if (d.success) { loadAccounts(); } else { alert(t('accounts.refreshFailed') + ': ' + d.error); }
            } catch (e) { alert(t('accounts.refreshFailed')); }
            if (card) card.classList.remove('loading');
        }
        async function showDetail(id) {
            const a = accountsData.find(x => x.id === id);
            if (!a) return;
            // Real-AWS overage tile: shows the cached upstream Overages switch
            // state + live billing $ (cap/rate/accumulated) and lets the operator
            // re-sync from AWS or flip the actual billing switch. Distinct from
            // the local AllowOverage routing knob above.
            const ovStatus = (a.overageStatus || 'UNKNOWN');
            const ovEnabled = ovStatus === 'ENABLED';
            const ovStatusCls = ovEnabled ? 'badge-warning' : (ovStatus === 'DISABLED' ? 'badge-info' : 'badge');
            const ovChecked = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : t('detail.overageNeverSynced');
            const fmtUsd = (n) => '$' + (Number(n) || 0).toFixed(2);
            const AWS_OVERAGE_SECTION_PLACEHOLDER =
                '<div class="detail-section"><h4>' + t('detail.awsOverage') + '</h4>' +
                '<div class="detail-grid">' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.awsOverageStatus') + '</div><div class="detail-value"><span class="badge ' + ovStatusCls + '">' + ovStatus + '</span></div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.awsOverageCurrent') + '</div><div class="detail-value">' + fmtUsd(a.currentOverages) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.awsOverageCap') + '</div><div class="detail-value">' + fmtUsd(a.overageCap) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.awsOverageRate') + '</div><div class="detail-value">' + fmtUsd(a.overageRate) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.awsOverageChecked') + '</div><div class="detail-value">' + ovChecked + '</div></div>' +
                '</div>' +
                '<div class="machine-id-row" style="margin-top:10px">' +
                '<button class="btn btn-sm btn-secondary" onclick="refreshAwsOverage(\'' + id + '\')">' + t('detail.awsOverageRefresh') + '</button>' +
                (ovEnabled
                    ? '<button class="btn btn-sm btn-secondary" onclick="setAwsOverage(\'' + id + '\',false)">' + t('detail.awsOverageDisable') + '</button>'
                    : '<button class="btn btn-sm btn-warning" onclick="setAwsOverage(\'' + id + '\',true)">' + t('detail.awsOverageEnable') + '</button>') +
                '</div>' +
                '<small style="color:#64748b;font-size:12px;margin-top:6px;display:block">' + t('detail.awsOverageHint') + '</small>' +
                '</div>';
            document.getElementById('detailBody').innerHTML =
                '<div class="detail-section"><h4>' + t('detail.basicInfo') + '</h4><div class="detail-grid">' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.email') + '</div><div class="detail-value">' + escapeHTML(getDisplayEmail(a.email, null)) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.userId') + '</div><div class="detail-value">' + escapeHTML(a.userId || '-') + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.authMethod') + '</div><div class="detail-value">' + escapeHTML(formatAuthMethod(a.provider || a.authMethod)) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.region') + '</div><div class="detail-value">' + escapeHTML(a.region || 'us-east-1') + '</div></div>' +
                '</div></div>' +
                '<div class="detail-section"><h4>' + t('detail.machineId') + '</h4><div class="machine-id-row">' +
                '<input type="text" id="machineIdInput" value="' + escapeHTML(a.machineId || '') + '" placeholder="UUID">' +
                '<button class="btn btn-sm btn-secondary" onclick="generateMachineId()">' + t('detail.generate') + '</button>' +
                '<button class="btn btn-sm btn-primary" onclick="saveMachineId(\'' + id + '\')">' + t('detail.save') + '</button>' +
                '</div></div>' +
                '<div class="detail-section"><h4>' + t('detail.weight') + '</h4>' +
                '<div class="form-group" style="margin-bottom:8px">' +
                '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10">' +
                '<small style="color:#64748b;font-size:12px;margin-top:4px;display:block">' + t('detail.weightHint') + '</small>' +
                '</div>' +
                '<button class="btn btn-sm btn-primary" onclick="saveWeight(\'' + id + '\')">' + t('detail.save') + '</button>' +
                '</div>' +
                '<div class="detail-section"><h4>' + t('detail.overage') + '</h4>' +
                '<div class="form-group">' +
                '<label style="display:flex;align-items:center;gap:10px"><label class="switch" style="margin:0"><input type="checkbox" id="allowOverageInput" ' + (a.allowOverage ? 'checked' : '') + '><span class="slider"></span></label><span>' + t('detail.allowOverage') + '</span></label>' +
                '</div>' +
                '<div class="form-group" style="margin-bottom:8px">' +
                '<label>' + t('detail.overageWeight') + '</label>' +
                '<input type="number" id="overageWeightInput" value="' + (a.overageWeight || 1) + '" min="1" max="10">' +
                '<small style="color:#64748b;font-size:12px;margin-top:4px;display:block">' + t('detail.overageHint') + '</small>' +
                '</div>' +
                '<button class="btn btn-sm btn-primary" onclick="saveOverageSettings(\'' + id + '\')">' + t('detail.save') + '</button>' +
                '</div>' +
                AWS_OVERAGE_SECTION_PLACEHOLDER +
                '<div class="detail-section"><h4>' + t('detail.proxyURL') + '</h4><div class="machine-id-row">' +
                '<input type="text" id="proxyURLInput" value="' + escapeHTML(a.proxyURL || '') + '" placeholder="socks5://host:port" style="flex:1">' +
                '<button class="btn btn-sm btn-primary" onclick="saveProxyURL(\'' + id + '\')">' + t('detail.save') + '</button>' +
                '</div><p style="color:#64748b;font-size:12px;margin-top:4px">' + t('detail.proxyHint') + '</p></div>' +
                '<div class="detail-section"><h4>' + t('detail.subscription') + '</h4><div class="detail-grid">' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.subscriptionType') + '</div><div class="detail-value">' + escapeHTML(a.subscriptionTitle || a.subscriptionType || '-') + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.tokenExpiry') + '</div><div class="detail-value">' + (a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.mainQuota') + '</div><div class="detail-value">' + (a.usageCurrent?.toFixed(1) || 0) + ' / ' + (a.usageLimit?.toFixed(0) || 0) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.resetDate') + '</div><div class="detail-value">' + escapeHTML(a.nextResetDate || '-') + '</div></div>' +
                (a.trialUsageLimit > 0 ? '<div class="detail-item"><div class="detail-label">' + t('detail.trialQuota') + '</div><div class="detail-value">' + (a.trialUsageCurrent?.toFixed(1) || 0) + ' / ' + (a.trialUsageLimit?.toFixed(0) || 0) + '</div></div>' +
                    '<div class="detail-item"><div class="detail-label">' + t('detail.trialStatus') + '</div><div class="detail-value">' + escapeHTML(a.trialStatus || '-') + '</div></div>' +
                    '<div class="detail-item"><div class="detail-label">' + t('detail.trialExpiry') + '</div><div class="detail-value">' + (a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-') + '</div></div>' : '') +
                '</div></div>' +
                '<div class="detail-section"><h4>' + t('detail.statistics') + '</h4><div class="detail-grid">' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.requestCount') + '</div><div class="detail-value">' + (a.requestCount || 0) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.errorCount') + '</div><div class="detail-value">' + (a.errorCount || 0) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.totalTokens') + '</div><div class="detail-value">' + formatNum(a.totalTokens || 0) + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('detail.totalCredits') + '</div><div class="detail-value">' + (a.totalCredits || 0).toFixed(2) + '</div></div>' +
                '</div></div>' +
                '<div class="detail-section"><h4>' + t('detail.models') + ' <button class="btn btn-sm btn-secondary" onclick="loadModels(\'' + id + '\')" style="margin-left:8px">' + t('detail.loadModels') + '</button> <button class="btn btn-sm btn-secondary" onclick="refreshAccountModels(\'' + id + '\')" style="margin-left:4px">' + t('detail.refreshModelCache') + '</button></h4><div id="modelsList" class="model-list"></div></div>';
            document.getElementById('detailModal').classList.add('active');
        }
        async function loadModels(id) {
            const container = document.getElementById('modelsList');
            container.innerHTML = '<p style="color:#64748b">' + t('detail.loading') + '</p>';
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/models', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                if (d.success && d.models) {
                    // 按 credit 比例排序（auto 模型优先）
                    const sortedModels = d.models.sort((a, b) => {
                        if (a.modelId === 'auto') return -1;
                        if (b.modelId === 'auto') return 1;
                        return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
                    });

                    container.innerHTML = sortedModels.map(m => {
                        const creditRatio = m.rateMultiplier || 1;
                        return '<div class="model-item">' +
                            '<div class="model-name">' + escapeHTML(m.modelId) + '</div>' +
                            '<div class="model-credit"><span class="credit-ratio">' + escapeHTML(String(creditRatio)) + 'x credit</span></div>' +
                            '<div class="model-info">' + escapeHTML(m.description || '') + '</div>' +
                            '</div>';
                    }).join('') || '<p style="color:#64748b">' + t('detail.noModels') + '</p>';
                } else {
                    container.innerHTML = '<p style="color:#ef4444">' + t('detail.loadFailed') + ': ' + escapeHTML(d.error || '') + '</p>';
                }
            } catch (e) { container.innerHTML = '<p style="color:#ef4444">' + t('detail.loadFailed') + '</p>'; }
        }
        function closeDetailModal() { document.getElementById('detailModal').classList.remove('active'); }
        async function generateMachineId() {
            try {
                const res = await fetch('/admin/api/generate-machine-id', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                if (d.machineId) document.getElementById('machineIdInput').value = d.machineId;
            } catch (e) { alert(t('detail.generateFailed')); }
        }
        async function saveMachineId(id) {
            const machineId = document.getElementById('machineIdInput').value.trim();
            if (machineId && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(machineId) && !/^[0-9a-f]{32}$/i.test(machineId)) {
                alert(t('detail.machineIdError')); return;
            }
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ machineId })
                });
                const d = await res.json();
                if (d.success) { alert(t('detail.saved')); loadAccounts(); } else { alert(t('detail.saveFailed') + ': ' + d.error); }
            } catch (e) { alert(t('detail.saveFailed')); }
        }
        async function saveWeight(id) {
            const weight = parseInt(document.getElementById('weightInput').value) || 0;
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ weight })
                });
                const d = await res.json();
                if (d.success) { alert(t('detail.saved')); loadAccounts(); } else { alert(t('detail.saveFailed') + ': ' + d.error); }
            } catch (e) { alert(t('detail.saveFailed')); }
        }
        async function saveOverageSettings(id) {
            const allowOverage = document.getElementById('allowOverageInput').checked;
            let overageWeight = parseInt(document.getElementById('overageWeightInput').value) || 1;
            overageWeight = Math.max(1, Math.min(10, overageWeight));
            document.getElementById('overageWeightInput').value = overageWeight;
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ allowOverage, overageWeight })
                });
                const d = await res.json();
                if (d.success) { alert(t('detail.saved')); loadAccounts(); } else { alert(t('detail.saveFailed') + ': ' + d.error); }
            } catch (e) { alert(t('detail.saveFailed')); }
        }
        async function saveProxyURL(id) {
            const proxyURL = document.getElementById('proxyURLInput').value.trim();
            if (proxyURL && !proxyURL.startsWith('socks5://') && !proxyURL.startsWith('socks5h://') && !proxyURL.startsWith('http://') && !proxyURL.startsWith('https://')) {
                alert('Format: socks5://host:port or http://host:port'); return;
            }
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ proxyURL })
                });
                const d = await res.json();
                if (d.success) { alert(t('detail.proxySaved')); loadAccounts(); } else { alert(t('detail.saveFailed') + ': ' + d.error); }
            } catch (e) { alert(t('detail.saveFailed')); }
        }
        // 测试日志
        let testLogs = [];
        
        function getTestLogPanel() {
            let panel = document.getElementById('testLogPanel');
            if (!panel) {
                const container = document.createElement('div');
                container.id = 'testLogPanel';
                container.style.cssText = 'position:fixed;bottom:0;right:0;width:420px;max-height:300px;background:#1e293b;color:#e2e8f0;border-radius:8px 0 0 0;box-shadow:-2px -2px 10px rgba(0,0,0,0.3);z-index:9999;display:flex;flex-direction:column;font-size:12px;font-family:monospace;';
                container.innerHTML = '<div style="display:flex;justify-content:space-between;align-items:center;padding:8px 12px;background:#334155;border-radius:8px 0 0 0;"><span style="font-weight:600;">📋 ' + t('accounts.testLog.title') + '</span><div><button onclick="clearTestLog()" style="background:#ef4444;color:#fff;border:none;border-radius:4px;padding:2px 8px;cursor:pointer;margin-right:4px;font-size:11px;">' + t('accounts.testLog.clear') + '</button><button onclick="toggleTestLog()" style="background:#64748b;color:#fff;border:none;border-radius:4px;padding:2px 8px;cursor:pointer;font-size:11px;">' + t('accounts.testLog.hide') + '</button></div></div><div id="testLogContent" style="overflow-y:auto;padding:8px 12px;flex:1;max-height:250px;"></div>';
                document.body.appendChild(container);
                panel = container;
            }
            panel.style.display = 'flex';
            return panel;
        }
        
        function addTestLog(msg, type) {
            const time = new Date().toLocaleTimeString();
            const color = type === 'ok' ? '#4ade80' : type === 'err' ? '#f87171' : type === 'info' ? '#60a5fa' : '#e2e8f0';
            testLogs.push({time, msg, color});
            if (testLogs.length > 100) testLogs.shift();
            getTestLogPanel();
            const logDiv = document.getElementById('testLogContent');
            logDiv.innerHTML += '<div style="color:' + color + ';margin-bottom:2px;">[' + time + '] ' + escapeHTML(msg) + '</div>';
            logDiv.scrollTop = logDiv.scrollHeight;
        }
        
        function clearTestLog() {
            testLogs = [];
            const logDiv = document.getElementById('testLogContent');
            if (logDiv) logDiv.innerHTML = '';
        }
        
        function toggleTestLog() {
            const panel = document.getElementById('testLogPanel');
            if (panel) panel.style.display = panel.style.display === 'none' ? 'flex' : 'none';
        }
        
        async function testAccount(id) {
            const btn = document.getElementById('test-' + id);
            if (!btn) return;
            const ex = document.getElementById('testModelPopover');
            if (ex) ex.remove();
            let models = [];
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/models/cached', {
                    headers: { 'X-Admin-Password': password }
                });
                const d = await res.json();
                models = Array.isArray(d.models) ? d.models : [];
            } catch (e) { /* fallback to empty */ }

            const rect = btn.getBoundingClientRect();
            const top = Math.min(rect.bottom + 6, window.innerHeight - 160);
            const left = Math.max(4, Math.min(rect.left, window.innerWidth - 280));
            let inputHtml;
            if (models.length > 0) {
                models.sort();
                inputHtml = '<select id="testModelChoice" style="width:100%;padding:6px 8px;border:1px solid var(--border-strong);border-radius:6px;font-size:13px;background:var(--surface);color:var(--text)">' +
                    models.map(function(m) { return '<option value="' + m + '">' + m + '</option>'; }).join('') +
                    '</select>';
            } else {
                inputHtml = '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" style="width:100%;padding:6px 8px;border:1px solid var(--border-strong);border-radius:6px;font-size:13px;box-sizing:border-box;background:var(--surface);color:var(--text)">';
            }
            const popover = document.createElement('div');
            popover.id = 'testModelPopover';
            popover.style.cssText = 'position:fixed;inset:0;z-index:9998;';
            popover.innerHTML =
                '<div onclick="event.stopPropagation()" style="position:fixed;top:' + top + 'px;left:' + left + 'px;background:var(--surface);color:var(--text);border:1px solid var(--border);border-radius:8px;box-shadow:var(--shadow-lg);padding:16px;width:264px;">' +
                '<div style="font-weight:600;font-size:13px;margin-bottom:10px;color:var(--text)">' + t('accounts.selectModel') + '</div>' +
                inputHtml +
                '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:12px;">' +
                '<button onclick="closeTestModelPopover()" style="padding:5px 14px;border:1px solid var(--border-strong);border-radius:5px;background:var(--surface);color:var(--text);cursor:pointer;font-size:12px;">' + t('common.cancel') + '</button>' +
                '<button onclick="confirmTestAccount(\'' + id + '\')" style="padding:5px 14px;border:none;border-radius:5px;background:var(--accent);color:var(--on-accent);cursor:pointer;font-size:12px;">' + t('accounts.test') + '</button>' +
                '</div></div>';
            popover.addEventListener('click', function(e) { if (e.target === popover) closeTestModelPopover(); });
            document.body.appendChild(popover);
        }
        function closeTestModelPopover() {
            const p = document.getElementById('testModelPopover');
            if (p) p.remove();
        }
        async function confirmTestAccount(id) {
            const choice = document.getElementById('testModelChoice');
            const model = (choice ? choice.value.trim() : '') || 'claude-sonnet-4';
            closeTestModelPopover();
            await runTestAccount(id, model);
        }
        async function runTestAccount(id, model) {
            const btn = document.getElementById('test-' + id);
            if (!btn) return;
            const origText = btn.textContent;
            btn.textContent = t('accounts.testing');
            btn.disabled = true;
            btn.style.background = '#6b7280';
            const acc = accountsData.find(a => a.id === id);
            const email = acc ? acc.email : id;
            const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
            addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
            try {
                const startTime = Date.now();
                const res = await fetch('/admin/api/accounts/' + id + '/test', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ model })
                });
                const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
                const d = await res.json();
                if (d.success) {
                    addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
                    btn.textContent = '✓';
                    btn.style.background = '#059669';
                } else {
                    addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || 'unknown'), 'err');
                    btn.textContent = '✗';
                    btn.style.background = '#dc2626';
                }
            } catch (e) {
                addTestLog(t('accounts.testLog.error', email, e.message), 'err');
                btn.textContent = '✗';
                btn.style.background = '#dc2626';
            }
            btn.disabled = false;
            setTimeout(() => { btn.textContent = origText; btn.style.background = '#2563eb'; }, 3000);
        }
        async function setAccountProxy(id, currentProxy) {
            const proxyURL = await uiPrompt(t('detail.proxyHint'), currentProxy || '', { placeholder: 'socks5://host:port' });
            if (proxyURL === null) return; // cancelled
            if (proxyURL && !proxyURL.startsWith('socks5://') && !proxyURL.startsWith('socks5h://') && !proxyURL.startsWith('http://') && !proxyURL.startsWith('https://')) {
                alert('Format: socks5://host:port or http://host:port'); return;
            }
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ proxyURL })
                });
                const d = await res.json();
                if (d.success) { alert(t('detail.proxySaved')); loadAccounts(); } else { alert(t('detail.saveFailed') + ': ' + d.error); }
            } catch (e) { alert(t('detail.saveFailed')); }
        }
        async function quickSetWeight(id, value) {
            const weight = parseInt(value) || 0;
            try {
                await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ weight })
                });
                const acc = accountsData.find(a => a.id === id);
                if (acc) acc.weight = weight;
            } catch (e) { /* silent */ }
        }

        // ==================== API Keys tab ====================
        let apikeysData = [];
        // loadApiKeysData fetches the API keys into the shared apikeysData
        // WITHOUT rendering — so the Analytics per-key breakdown can refresh the
        // underlying totals on its own poll without touching the API Keys list.
        async function loadApiKeysData() {
            const res = await fetch('/admin/api/apikeys', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            apikeysData = d.keys || [];
        }
        async function loadApiKeys() {
            await loadApiKeysData();
            renderApiKeys();
        }
        function renderApiKeys() {
            const container = document.getElementById('apikeysList');
            if (!container) return;
            if (apikeysData.length === 0) {
                container.innerHTML = '<div class="empty">' + t('apikeys.empty') + '</div>';
                return;
            }
            container.innerHTML = apikeysData.map(k => {
                const reqUsed = k.dailyRequests || 0;
                const reqLim = k.dailyReqLimit || 0;
                const tokUsed = k.dailyTokens || 0;
                const tokLim = k.dailyTokLimit || 0;
                const credUsed = k.dailyCredits || 0;
                const credLim = k.dailyCredLimit || 0;
                const statusClass = !k.enabled ? 'is-disabled' : '';
                const statusBadge = !k.enabled ? '<span class="badge badge-error" data-i18n="apikeys.revoked">' + t('apikeys.revoked') + '</span>' : '<span class="badge badge-success">' + t('apikeys.active') + '</span>';
                const limitRow = (label, used, lim) => {
                    if (lim === 0) return '<div class="usage-text"><span>' + label + '</span><span>' + used.toFixed(0) + ' / ∞</span></div>';
                    const pct = Math.min(100, used / lim * 100);
                    const cls = pct > 90 ? 'critical' : pct > 70 ? 'high' : '';
                    return '<div class="account-usage"><div class="usage-label">' + label + '</div>' +
                        '<div class="usage-bar"><div class="usage-fill ' + cls + '" style="width:' + pct + '%"></div></div>' +
                        '<div class="usage-text"><span>' + used.toFixed(used < 10 ? 2 : 0) + ' / ' + lim.toFixed(0) + '</span><span>' + pct.toFixed(0) + '%</span></div></div>';
                };
                const periodLabel = t('apikeys.period.' + (k.resetPeriod || 'daily'));
                const tzLabel = k.resetTZ || 'UTC';
                const lastUsed = k.lastUsedAt ? new Date(k.lastUsedAt * 1000).toLocaleString() : '-';
                const created = k.createdAt ? new Date(k.createdAt * 1000).toLocaleString() : '-';
                // Compose flags row showing rate-limit / lifetime / expiry policies
                const flags = [];
                if (k.minuteReqLimit > 0) flags.push(t('apikeys.minuteReq') + ': ' + (k.minuteRequests || 0) + ' / ' + k.minuteReqLimit);
                if (k.hourReqLimit > 0) flags.push(t('apikeys.hourReq') + ': ' + (k.hourRequests || 0) + ' / ' + k.hourReqLimit);
                if (k.lifetimeReqLimit > 0) flags.push(t('apikeys.lifetimeReq') + ': ' + (k.totalRequests || 0) + ' / ' + k.lifetimeReqLimit);
                if (k.lifetimeTokLimit > 0) flags.push(t('apikeys.lifetimeTok') + ': ' + (k.totalTokens || 0) + ' / ' + k.lifetimeTokLimit);
                if (k.lifetimeCredLimit > 0) flags.push(t('apikeys.lifetimeCred') + ': ' + (k.totalCredits || 0).toFixed(1) + ' / ' + k.lifetimeCredLimit);
                if (k.expiresAt > 0) flags.push(t('apikeys.expiresAt') + ': ' + new Date(k.expiresAt * 1000).toLocaleString());
                if (k.lazyExpirySeconds > 0) {
                    const days = (k.lazyExpirySeconds / 86400).toFixed(0);
                    flags.push(t('apikeys.lazyExpiryFlag') + ': ' + days + 'd' + (k.firstUsedAt ? ' (' + t('apikeys.fromFirstUse') + ')' : ''));
                }
                const flagsRow = flags.length === 0 ? '' :
                    '<div style="display:flex;flex-wrap:wrap;gap:6px;padding:6px 0;font-size:11px">' +
                    flags.map(f => '<span style="background:var(--surface-3);border:1px solid var(--border);padding:2px 9px;border-radius:999px;color:var(--muted)">' + escapeHTML(f) + '</span>').join('') +
                    '</div>';
                return '<div class="account-card ' + statusClass + '">' +
                    '<div class="account-header">' +
                    '<div class="account-info">' +
                    '<div class="account-email">' + escapeHTML(k.name || '(unnamed)') + ' ' + statusBadge + ' <span style="color:var(--faint);font-size:11px;margin-left:6px">' + escapeHTML(periodLabel) + ' · ' + escapeHTML(tzLabel) + '</span></div>' +
                    '<div class="account-meta"><code id="apikey-' + k.id + '" style="background:var(--surface-3);border:1px solid var(--border);padding:2px 7px;border-radius:5px;font-size:11px;font-family:var(--mono)">' + escapeHTML(k.key) + '</code>' +
                    '<button class="btn btn-sm btn-secondary" style="margin-left:6px;padding:3px 9px;font-size:11px" title="' + t('common.copy') + '" onclick="copyApiKey(\'' + k.id + '\')">📋</button>' +
                    '<span style="font-size:11px;color:var(--faint);margin-left:8px">' + t('apikeys.maskedHint') + '</span></div>' +
                    '</div>' +
                    '<div class="account-actions">' +
                    '<button class="btn btn-sm btn-secondary" onclick="editApiKey(\'' + k.id + '\')">' + t('common.edit') + '</button>' +
                    (k.enabled
                        ? '<button class="btn btn-sm btn-secondary" onclick="toggleApiKey(\'' + k.id + '\', false)">' + t('apikeys.revoke') + '</button>'
                        : '<button class="btn btn-sm btn-success" onclick="toggleApiKey(\'' + k.id + '\', true)">' + t('apikeys.enable') + '</button>') +
                    '<button class="btn btn-sm btn-danger" onclick="deleteApiKey(\'' + k.id + '\')">' + t('common.delete') + '</button>' +
                    '</div></div>' +
                    flagsRow +
                    limitRow(t('apikeys.dailyRequests'), reqUsed, reqLim) +
                    limitRow(t('apikeys.dailyTokens'), tokUsed, tokLim) +
                    limitRow(t('apikeys.dailyCredits'), credUsed, credLim) +
                    '<div class="account-stats" style="grid-template-columns:repeat(4,1fr)">' +
                    '<div class="account-stat"><div class="account-stat-value">' + (k.totalRequests || 0) + '</div><div class="account-stat-label">' + t('apikeys.totalReq') + '</div></div>' +
                    '<div class="account-stat"><div class="account-stat-value">' + formatNum(k.totalTokens || 0) + '</div><div class="account-stat-label">' + t('apikeys.totalTok') + '</div></div>' +
                    '<div class="account-stat"><div class="account-stat-value">' + (k.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + t('apikeys.totalCred') + '</div></div>' +
                    '<div class="account-stat"><div class="account-stat-value" style="font-size:11px">' + lastUsed + '</div><div class="account-stat-label">' + t('apikeys.lastUsed') + '</div></div>' +
                    '</div></div>';
            }).join('');
        }
        function buildApiKeyForm(prefix, k) {
            // prefix is "new" or "ed". k is undefined for create, the existing key for edit.
            const v = (path, def) => (k && k[path] != null ? k[path] : def);
            // Full HTML-entity escape (shared helper), not quote-only — closes the
            // attribute-breakout XSS where a key name like x"><img onerror=…> would
            // otherwise inject markup through the value="" interpolations below.
            const escape = s => escapeHTML(s);
            const tzList = ['UTC', 'Asia/Singapore', 'Asia/Tokyo', 'Asia/Shanghai', 'Asia/Kolkata', 'Asia/Jakarta',
                'Europe/London', 'Europe/Paris', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles',
                'Australia/Sydney'];
            const curTZ = v('resetTZ', 'UTC');
            const tzOptions = tzList.map(z => '<option value="' + z + '"' + (z === curTZ ? ' selected' : '') + '>' + z + '</option>').join('');
            const curPeriod = v('resetPeriod', 'daily');
            const expiresAt = v('expiresAt', 0);
            // datetime-local expects local time formatted as YYYY-MM-DDTHH:mm.
            // toISOString() returns UTC and would drift the field by the user's
            // TZ offset on every edit roundtrip — format manually instead.
            const expiresAtIso = (() => {
                if (expiresAt <= 0) return '';
                const d = new Date(expiresAt * 1000);
                const pad = n => String(n).padStart(2, '0');
                return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
                       'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
            })();
            const lazy = v('lazyExpirySeconds', 0);
            const lazyDays = lazy > 0 ? (lazy / 86400) : '';
            const sectionStyle = 'border:1px solid var(--border);border-radius:var(--r-md);padding:14px;margin-bottom:12px;background:var(--surface-2)';
            const sectionTitle = (txt) => '<div style="font-weight:620;color:var(--text);font-size:13px;margin-bottom:10px">' + txt + '</div>';
            return (
                // Basic
                '<div style="' + sectionStyle + '">' + sectionTitle(t('apikeys.section.basic')) +
                '<div class="form-group"><label>' + t('apikeys.name') + '</label><input type="text" id="' + prefix + 'KeyName" value="' + escape(v('name', '')) + '" placeholder="my-app"></div>' +
                '<div class="form-group"><label>Allowed Models</label>' +
                '<div id="' + prefix + 'KeyModelsChecks" data-current="' + escape((Array.isArray(v('models', [])) ? v('models', []) : []).join(',')) + '" style="border:1px solid var(--border-strong);border-radius:var(--r-sm);padding:8px;background:var(--surface);max-height:180px;overflow-y:auto;font-size:12px">Loading models…</div>' +
                '<input type="text" id="' + prefix + 'KeyModelsExtra" value="" placeholder="extra ids not in the list, comma-separated (rare)" style="margin-top:6px;font-size:12px">' +
                '<small class="field-hint">Tick the models this key may invoke. Default (none ticked) = all models allowed. Dotted/dashed forms (claude-opus-4.7 ↔ claude-opus-4-7) match interchangeably; calls to disallowed models are rejected before any upstream account is touched, and /v1/models returns only the allowed entries when this key is used. The extra-ids field is for models not yet in the catalog (e.g. preview ids).</small>' +
                '</div>' +
                '</div>' +
                // Periodic
                '<div style="' + sectionStyle + '">' + sectionTitle(t('apikeys.section.periodic')) +
                '<div class="form-group" style="display:grid;grid-template-columns:1fr 1fr;gap:8px">' +
                '<div><label>' + t('apikeys.resetPeriod') + '</label><select id="' + prefix + 'KeyPeriod">' +
                ['daily', 'weekly', 'monthly'].map(p => '<option value="' + p + '"' + (p === curPeriod ? ' selected' : '') + '>' + t('apikeys.period.' + p) + '</option>').join('') +
                '</select></div>' +
                '<div><label>' + t('apikeys.resetTZ') + '</label><select id="' + prefix + 'KeyTZ">' + tzOptions + '</select></div>' +
                '</div>' +
                '<div class="form-group"><label>' + t('apikeys.dailyRequests') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyReqLim" value="' + (v('dailyReqLimit', 0)) + '" min="0"></div>' +
                '<div class="form-group"><label>' + t('apikeys.dailyTokens') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyTokLim" value="' + (v('dailyTokLimit', 0)) + '" min="0"></div>' +
                '<div class="form-group"><label>' + t('apikeys.dailyCredits') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyCredLim" value="' + (v('dailyCredLimit', 0)) + '" min="0" step="0.01"></div>' +
                '</div>' +
                // Burst
                '<div style="' + sectionStyle + '">' + sectionTitle(t('apikeys.section.burst')) +
                '<div class="form-group" style="display:grid;grid-template-columns:1fr 1fr;gap:8px">' +
                '<div><label>' + t('apikeys.minuteReq') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyMin" value="' + (v('minuteReqLimit', 0)) + '" min="0"></div>' +
                '<div><label>' + t('apikeys.hourReq') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyHour" value="' + (v('hourReqLimit', 0)) + '" min="0"></div>' +
                '</div></div>' +
                // Lifetime
                '<div style="' + sectionStyle + '">' + sectionTitle(t('apikeys.section.lifetime')) +
                '<small class="field-hint" style="margin-top:0;margin-bottom:8px">' + t('apikeys.lifetimeHint') + '</small>' +
                '<div class="form-group"><label>' + t('apikeys.lifetimeReq') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyLifeReq" value="' + (v('lifetimeReqLimit', 0)) + '" min="0"></div>' +
                '<div class="form-group"><label>' + t('apikeys.lifetimeTok') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyLifeTok" value="' + (v('lifetimeTokLimit', 0)) + '" min="0"></div>' +
                '<div class="form-group"><label>' + t('apikeys.lifetimeCred') + ' (0 = ∞)</label><input type="number" id="' + prefix + 'KeyLifeCred" value="' + (v('lifetimeCredLimit', 0)) + '" min="0" step="0.01"></div>' +
                '</div>' +
                // Expiry
                '<div style="' + sectionStyle + '">' + sectionTitle(t('apikeys.section.expiry')) +
                '<div class="form-group"><label>' + t('apikeys.absoluteExpiry') + '</label><input type="datetime-local" id="' + prefix + 'KeyExpAbs" value="' + expiresAtIso + '"><small class="field-hint">' + t('apikeys.absoluteHint') + '</small></div>' +
                '<div class="form-group"><label>' + t('apikeys.lazyExpiry') + '</label><input type="number" id="' + prefix + 'KeyLazyDays" value="' + lazyDays + '" min="0" step="1" placeholder="0"><small class="field-hint">' + t('apikeys.lazyHint') + '</small></div>' +
                '</div>'
            );
        }
        // Catalog of every available model id, used to render the
        // "Allowed Models" checkbox grid in the API key form. Cached for
        // the page lifetime so opening multiple key dialogs doesn't
        // re-hit /admin/api/available-models. Refreshed when the dialog
        // is opened on a cold cache or after a manual reload.
        let availableModelsCache = null;
        async function fetchAvailableModels(force) {
            if (availableModelsCache && !force) return availableModelsCache;
            try {
                const res = await fetch('/admin/api/available-models', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                availableModelsCache = Array.isArray(d.models) ? d.models : [];
            } catch (e) {
                availableModelsCache = [];
            }
            return availableModelsCache;
        }
        async function populateModelCheckboxes(prefix) {
            const host = document.getElementById(prefix + 'KeyModelsChecks');
            if (!host) return;
            const currentRaw = host.getAttribute('data-current') || '';
            // Selected ids the operator persisted; lowercased for the
            // case-insensitive match against catalog ids.
            const selected = new Set(currentRaw.split(',').map(s => s.trim().toLowerCase()).filter(Boolean));
            const models = await fetchAvailableModels(false);
            if (!models.length) {
                host.innerHTML = '<div style="color:#94a3b8;padding:4px">No models in catalog yet — try saving and reopening once accounts have refreshed, or use the extra-ids field below.</div>';
                return;
            }
            // The catalog includes both the canonical dashed form and the
            // dotted Kiro alias for every model (Phase 1 dedup leaves them
            // both in /v1/models). The whitelist matcher treats them as the
            // same model, so showing both as separate checkboxes would
            // double the rows. Collapse on the canonical (dot→dash) form.
            const canonical = new Map();
            for (const id of models) {
                const key = id.toLowerCase().replace(/\./g, '-');
                if (!canonical.has(key)) canonical.set(key, id);
            }
            // Detect "extra" ids the user previously had selected that
            // aren't in the catalog — render them in the extra-ids
            // textbox so they're not silently lost on save.
            const catalogKeys = new Set(canonical.keys());
            const extras = [];
            for (const sel of selected) {
                const k = sel.replace(/\./g, '-');
                if (!catalogKeys.has(k)) extras.push(sel);
            }
            const extraInput = document.getElementById(prefix + 'KeyModelsExtra');
            if (extraInput && extras.length > 0) extraInput.value = extras.join(', ');

            // Quick "all / none" controls + the checkboxes themselves.
            const ids = Array.from(canonical.values()).sort();
            // Pre-tick rule: a stored id matches a displayed catalog id
            // when their canonical forms are equal. Canonical = lowercase
            // + every "." → "-" (mirrors the catalog dedup key built
            // above and the request-time alias matcher in
            // config.IsModelAllowedForAPIKey). Without this, a key
            // persisted as "claude-opus-4.7" against a catalog row
            // displayed as "claude-opus-4-7" rendered unchecked, and
            // saving without other edits would silently strip the
            // allowlist entry — a security regression.
            const selectedCanon = new Set(
                Array.from(selected).map(s => s.replace(/\./g, '-'))
            );
            let html = '<div style="display:flex;gap:8px;margin-bottom:6px;font-size:11px">' +
                '<button type="button" class="btn btn-sm btn-secondary" style="padding:2px 8px;font-size:11px" onclick="toggleAllModelChecks(\'' + prefix + '\', true)">Select all</button>' +
                '<button type="button" class="btn btn-sm btn-secondary" style="padding:2px 8px;font-size:11px" onclick="toggleAllModelChecks(\'' + prefix + '\', false)">Clear</button>' +
                '<button type="button" class="btn btn-sm btn-secondary" style="padding:2px 8px;font-size:11px" onclick="reloadModelCheckboxes(\'' + prefix + '\')" title="Re-fetch the catalog from the server (use after adding new Kiro accounts)">↻</button>' +
                '<span style="color:#94a3b8">No selection = all allowed (default)</span></div>' +
                '<div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:4px">';
            for (const id of ids) {
                const idCanon = id.toLowerCase().replace(/\./g, '-');
                const checked = selectedCanon.has(idCanon);
                html += '<label style="display:flex;align-items:center;gap:6px;font-size:12px;cursor:pointer">' +
                    '<input type="checkbox" data-model-id="' + escapeHTML(id) + '"' + (checked ? ' checked' : '') + '>' +
                    '<span style="font-family:monospace">' + escapeHTML(id) + '</span></label>';
            }
            html += '</div>';
            host.innerHTML = html;
        }
        function toggleAllModelChecks(prefix, on) {
            const host = document.getElementById(prefix + 'KeyModelsChecks');
            if (!host) return;
            host.querySelectorAll('input[type=checkbox]').forEach(cb => { cb.checked = on; });
        }
        // reloadModelCheckboxes drops the cached catalog and re-renders the
        // grid. Useful when the operator just added a Kiro account whose
        // models weren't in the cache when the dialog opened.
        async function reloadModelCheckboxes(prefix) {
            availableModelsCache = null;
            // Persist whatever was ticked + the extras textbox so the
            // re-render restores them. populateModelCheckboxes reads
            // data-current and the extras input as the source of truth.
            const host = document.getElementById(prefix + 'KeyModelsChecks');
            if (host) {
                const ticked = Array.from(host.querySelectorAll('input[type=checkbox]:checked'))
                    .map(cb => cb.getAttribute('data-model-id') || '').filter(Boolean);
                const extra = (document.getElementById(prefix + 'KeyModelsExtra')?.value || '').trim();
                const extras = extra === '' ? [] : extra.split(/[,\n]/).map(s => s.trim()).filter(Boolean);
                host.setAttribute('data-current', ticked.concat(extras).join(','));
            }
            await populateModelCheckboxes(prefix);
        }
        function readApiKeyForm(prefix) {
            const tzVal = document.getElementById(prefix + 'KeyTZ').value || 'UTC';
            const periodVal = document.getElementById(prefix + 'KeyPeriod').value || 'daily';
            const expIso = document.getElementById(prefix + 'KeyExpAbs').value;
            const expiresAt = expIso ? Math.floor(new Date(expIso).getTime() / 1000) : 0;
            const lazyDays = parseFloat(document.getElementById(prefix + 'KeyLazyDays').value) || 0;
            const lazyExpirySeconds = lazyDays > 0 ? Math.floor(lazyDays * 86400) : 0;
            // Allowed-models input: collect ticked checkboxes from the
            // catalog grid plus any "extra" ids in the secondary textbox
            // (operators may pre-authorize models not yet in the cached
            // catalog, e.g. preview names). Empty result = no
            // restriction (default: all models allowed).
            const checkHost = document.getElementById(prefix + 'KeyModelsChecks');
            const ticked = checkHost
                ? Array.from(checkHost.querySelectorAll('input[type=checkbox]:checked')).map(cb => cb.getAttribute('data-model-id') || '').filter(Boolean)
                : [];
            const extraRaw = (document.getElementById(prefix + 'KeyModelsExtra')?.value || '').trim();
            const extra = extraRaw === '' ? [] : extraRaw.split(/[,\n]/).map(s => s.trim()).filter(Boolean);
            const seenModel = new Set();
            const models = [];
            for (const id of ticked.concat(extra)) {
                const key = id.toLowerCase();
                if (seenModel.has(key)) continue;
                seenModel.add(key);
                models.push(id);
            }
            return {
                name: document.getElementById(prefix + 'KeyName').value.trim(),
                models,
                resetPeriod: periodVal,
                resetTZ: tzVal,
                dailyReqLimit: parseInt(document.getElementById(prefix + 'KeyReqLim').value) || 0,
                dailyTokLimit: parseInt(document.getElementById(prefix + 'KeyTokLim').value) || 0,
                dailyCredLimit: parseFloat(document.getElementById(prefix + 'KeyCredLim').value) || 0,
                minuteReqLimit: parseInt(document.getElementById(prefix + 'KeyMin').value) || 0,
                hourReqLimit: parseInt(document.getElementById(prefix + 'KeyHour').value) || 0,
                lifetimeReqLimit: parseInt(document.getElementById(prefix + 'KeyLifeReq').value) || 0,
                lifetimeTokLimit: parseInt(document.getElementById(prefix + 'KeyLifeTok').value) || 0,
                lifetimeCredLimit: parseFloat(document.getElementById(prefix + 'KeyLifeCred').value) || 0,
                expiresAt,
                lazyExpirySeconds
            };
        }
        function showCreateApiKey() {
            const body = document.getElementById('modalBody');
            document.getElementById('modalTitle').textContent = t('apikeys.create');
            body.innerHTML = buildApiKeyForm('new', null) +
                '<div class="modal-footer"><button class="btn btn-primary" onclick="submitCreateApiKey()">' + t('common.create') + '</button></div>';
            document.getElementById('addModal').classList.add('active');
            populateModelCheckboxes('new');
        }
        async function submitCreateApiKey() {
            const payload = readApiKeyForm('new');
            const res = await fetch('/admin/api/apikeys', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify(payload)
            });
            const d = await res.json();
            if (d.id) {
                // Patch the new key with the burst/lifetime/expiry/period fields the
                // backend create endpoint doesn't accept directly (kept narrow for
                // backward compat). One follow-up PUT covers the rest.
                await fetch('/admin/api/apikeys/' + d.id, {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify(payload)
                });
                closeModal();
                alert(t('apikeys.created') + ': ' + d.key);
                loadApiKeys();
            } else {
                alert(d.error || 'Failed');
            }
        }
        function editApiKey(id) {
            const k = apikeysData.find(x => x.id === id);
            if (!k) return;
            const body = document.getElementById('modalBody');
            document.getElementById('modalTitle').textContent = t('common.edit') + ': ' + (k.name || k.id);
            body.innerHTML = buildApiKeyForm('ed', k) +
                '<div class="modal-footer"><button class="btn btn-primary" onclick="submitEditApiKey(\'' + id + '\')">' + t('common.save') + '</button></div>';
            document.getElementById('addModal').classList.add('active');
            populateModelCheckboxes('ed');
        }
        async function submitEditApiKey(id) {
            const payload = readApiKeyForm('ed');
            const res = await fetch('/admin/api/apikeys/' + id, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify(payload)
            });
            const d = await res.json();
            if (d.success) { closeModal(); loadApiKeys(); }
            else alert(d.error || 'Failed');
        }
        async function toggleApiKey(id, enabled) {
            const res = await fetch('/admin/api/apikeys/' + id, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ enabled })
            });
            if ((await res.json()).success) loadApiKeys();
        }
        async function deleteApiKey(id) {
            if (!await uiConfirm(t('apikeys.confirmDelete'), { danger: true })) return;
            const res = await fetch('/admin/api/apikeys/' + id, {
                method: 'DELETE',
                headers: { 'X-Admin-Password': password }
            });
            if ((await res.json()).success) loadApiKeys();
        }

        // ==================== Analytics tab ====================
        async function loadAnalytics() {
            const res = await fetch('/admin/api/modelstats', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            renderModelStats(d.models || {});
            // Refresh the per-key totals the breakdown renders from. renderKeyStats
            // reads the global apikeysData, which only the API Keys tab otherwise
            // refreshes — without this the per-key panel re-renders every 5s from
            // stale numbers while sitting on Analytics. Best-effort: a failed key
            // fetch just leaves the prior data and still renders.
            try { await loadApiKeysData(); } catch (e) { /* keep prior key data */ }
            renderKeyStats();
            renderAnalyticsTrend();
        }
        // renderAnalyticsTrend draws the 14-day requests/credits sparklines on the
        // Analytics tab, reusing the same history endpoint + sparkline helper the
        // Overview uses. Theme-aware colors are read live so a theme toggle repaints.
        async function renderAnalyticsTrend() {
            const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || '#6366f1';
            const success = getComputedStyle(document.documentElement).getPropertyValue('--success').trim() || '#22c55e';
            try {
                const res = await fetch('/admin/api/stats/history?scope=global&days=14', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                const entries = d.entries || [];
                const reqs = entries.map(e => e.requests || 0);
                const creds = entries.map(e => +(e.credits || 0));
                const setText = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };
                setText('anReqValue', formatNum(reqs.reduce((a, b) => a + b, 0)));
                setText('anCredValue', creds.reduce((a, b) => a + b, 0).toFixed(1));
                const rs = document.getElementById('anReqSpark'); if (rs) rs.innerHTML = sparkline(reqs, accent);
                const cs = document.getElementById('anCredSpark'); if (cs) cs.innerHTML = sparkline(creds, success);
                const first = entries[0]?.date || '', last = entries[entries.length - 1]?.date || '';
                setText('anReqStart', first); setText('anReqEnd', last);
                setText('anCredStart', first); setText('anCredEnd', last);
            } catch (e) { /* leave charts empty on error */ }
        }
        function renderModelStats(models) {
            const container = document.getElementById('modelStatsList');
            if (!container) return;
            const entries = Object.entries(models).sort((a, b) => (b[1].credits || 0) - (a[1].credits || 0));
            if (entries.length === 0) {
                container.innerHTML = '<div class="empty">' + t('analytics.empty') + '</div>';
                return;
            }
            const maxCredits = Math.max(...entries.map(e => e[1].credits || 0)) || 1;
            container.innerHTML = entries.map(([model, s]) => {
                const pct = ((s.credits || 0) / maxCredits) * 100;
                return '<div style="margin-bottom:14px">' +
                    '<div style="display:flex;justify-content:space-between;font-size:13px;margin-bottom:5px;gap:10px">' +
                    '<span style="font-weight:540;font-family:var(--mono)">' + escapeHTML(model) + '</span>' +
                    '<span style="color:var(--muted);font-variant-numeric:tabular-nums">' + (s.requests || 0) + ' req · ' + formatNum(s.tokens || 0) + ' tok · ' + (s.credits || 0).toFixed(2) + ' cr</span>' +
                    '</div>' +
                    '<div class="usage-bar"><div class="usage-fill" style="width:' + pct + '%"></div></div>' +
                    renderEffortBreakdown(s.effort) +
                    '</div>';
            }).join('');
        }
        // renderEffortBreakdown renders the per-reasoning-effort split for one
        // model as a row of compact chips (one per level). Levels are ordered
        // low -> max with "default" (no graded effort) last. Returns '' when
        // the only bucket is "default" — there's nothing interesting to show if
        // the model was never driven at a graded effort.
        function renderEffortBreakdown(effort) {
            if (!effort) return '';
            const order = { low: 1, medium: 2, high: 3, xhigh: 4, max: 5, default: 99 };
            const levels = Object.keys(effort).sort((a, b) => (order[a] || 50) - (order[b] || 50));
            const graded = levels.filter(l => l !== 'default' && (effort[l].requests || 0) > 0);
            if (graded.length === 0) return '';
            const chips = levels.filter(l => (effort[l].requests || 0) > 0).map(l => {
                const e = effort[l];
                const isDefault = l === 'default';
                const label = isDefault ? t('analytics.effortDefault') : l;
                const bg = isDefault ? 'var(--surface-3)' : 'var(--accent-soft)';
                const fg = isDefault ? 'var(--muted)' : 'var(--accent)';
                const bd = isDefault ? 'var(--border)' : 'var(--accent-border)';
                return '<span title="' + (e.requests || 0) + ' req · ' + formatNum(e.tokens || 0) + ' tok · ' + (e.credits || 0).toFixed(2) + ' cr" ' +
                    'style="display:inline-flex;gap:6px;align-items:center;background:' + bg + ';color:' + fg + ';border:1px solid ' + bd + ';border-radius:999px;padding:2px 9px;font-size:11px;font-variant-numeric:tabular-nums">' +
                    '<b style="font-weight:600;text-transform:uppercase;letter-spacing:.3px">' + escapeHTML(label) + '</b>' +
                    '<span style="opacity:.75">' + (e.requests || 0) + ' req · ' + formatNum(e.tokens || 0) + ' tok</span>' +
                    '</span>';
            }).join('');
            return '<div style="display:flex;flex-wrap:wrap;gap:6px;margin-top:8px">' +
                '<span style="font-size:11px;color:var(--muted);align-self:center">' + t('analytics.effortLabel') + '</span>' + chips +
                '</div>';
        }
        function renderKeyStats() {
            const container = document.getElementById('keyStatsList');
            if (!container) return;
            const keys = (apikeysData || []).filter(k => (k.totalRequests || 0) > 0);
            if (keys.length === 0) {
                container.innerHTML = '<div class="empty">' + t('analytics.noKeys') + '</div>';
                return;
            }
            keys.sort((a, b) => (b.totalCredits || 0) - (a.totalCredits || 0));
            const maxCredits = Math.max(...keys.map(k => k.totalCredits || 0)) || 1;
            container.innerHTML = keys.map(k => {
                const pct = ((k.totalCredits || 0) / maxCredits) * 100;
                return '<div style="margin-bottom:14px">' +
                    '<div style="display:flex;justify-content:space-between;font-size:13px;margin-bottom:5px;gap:10px">' +
                    '<span style="font-weight:540">' + escapeHTML(k.name || k.id) + '</span>' +
                    '<span style="color:var(--muted);font-variant-numeric:tabular-nums">' + (k.totalRequests || 0) + ' req · ' + formatNum(k.totalTokens || 0) + ' tok · ' + (k.totalCredits || 0).toFixed(2) + ' cr</span>' +
                    '</div>' +
                    '<div class="usage-bar"><div class="usage-fill" style="width:' + pct + '%"></div></div>' +
                    '</div>';
            }).join('');
        }

        async function loadSettings() {
            const res = await fetch('/admin/api/settings', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            document.getElementById('requireApiKey').checked = d.requireApiKey;
            document.getElementById('apiKeyInput').value = d.apiKey || '';
            document.getElementById('allowOverUsage').checked = d.allowOverUsage || false;
            const wsEnabled = document.getElementById('webSearchEnabled');
            if (wsEnabled) {
                wsEnabled.checked = d.webSearchEnabled || false;
                const wsProvider = document.getElementById('webSearchProvider');
                if (wsProvider) wsProvider.value = d.webSearchProvider || 'kiro';
                window._webSearchKeySet = !!d.webSearchApiKeySet;
                const wsKey = document.getElementById('webSearchApiKey');
                if (wsKey) wsKey.value = '';
                onWebSearchProviderChange();
            }
            loadThinkingConfig();
            loadEndpointConfig();
            loadProxyConfig();
            loadPromptFilter();
        }
        function onWebSearchProviderChange() {
            const provider = document.getElementById('webSearchProvider').value;
            const keyRow = document.getElementById('webSearchKeyRow');
            const needsKey = provider !== 'kiro';
            if (keyRow) keyRow.classList.toggle('hidden', !needsKey);
            const hint = document.getElementById('webSearchKeyHint');
            if (hint) hint.textContent = needsKey ? '(' + provider + ')' : '';
            const state = document.getElementById('webSearchKeyState');
            if (state) state.textContent = window._webSearchKeySet ? 'A key is saved. Leave blank to keep it.' : 'No key saved yet.';
        }
        async function saveWebSearchConfig() {
            const payload = {
                webSearchEnabled: document.getElementById('webSearchEnabled').checked,
                webSearchProvider: document.getElementById('webSearchProvider').value,
            };
            const key = document.getElementById('webSearchApiKey').value;
            if (key) payload.webSearchApiKey = key;
            const res = await fetch('/admin/api/settings', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify(payload)
            });
            if (res.ok) {
                alert('Web search settings saved');
                loadSettings();
            } else {
                alert('Failed to save web search settings');
            }
        }
        async function loadThinkingConfig() {
            const res = await fetch('/admin/api/thinking', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            document.getElementById('thinkingSuffix').value = d.suffix || '-thinking';
            document.getElementById('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
            document.getElementById('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
        }
        async function saveThinkingConfig() {
            const res = await fetch('/admin/api/thinking', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ suffix: document.getElementById('thinkingSuffix').value || '-thinking', openaiFormat: document.getElementById('openaiThinkingFormat').value, claudeFormat: document.getElementById('claudeThinkingFormat').value })
            });
            const d = await res.json();
            if (d.success) { alert(t('settings.thinkingSaved')); } else { alert(t('common.saveFailed') + ': ' + d.error); }
        }
        async function loadEndpointConfig() {
            const res = await fetch('/admin/api/endpoint', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            const regionEl = document.getElementById('kiroAPIRegion');
            if (regionEl) {
                const r = (d.region || 'us-east-1').toLowerCase();
                if (!Array.from(regionEl.options).some(o => o.value === r)) {
                    const opt = document.createElement('option');
                    opt.value = r;
                    opt.textContent = r + ' (custom)';
                    regionEl.appendChild(opt);
                }
                regionEl.value = r;
            }
            const regionsEl = document.getElementById('kiroAPIRegions');
            if (regionsEl) {
                regionsEl.value = Array.isArray(d.regions) ? d.regions.join(', ') : '';
            }
            const stratEl = document.getElementById('poolStrategy');
            if (stratEl) {
                stratEl.value = d.poolStrategy || 'fast';
            }
            const fastConcEl = document.getElementById('poolFastConcurrency');
            if (fastConcEl) {
                fastConcEl.value = d.poolFastConcurrency || '';
            }
            const initConcEl = document.getElementById('poolInitialConcurrency');
            if (initConcEl) {
                initConcEl.value = d.poolInitialConcurrency || '';
            }
            const maxConcEl = document.getElementById('poolMaxConcurrency');
            if (maxConcEl) {
                maxConcEl.value = d.poolMaxConcurrency || '';
            }
        }
        async function saveEndpointConfig() {
            const body = {};
            const regionEl = document.getElementById('kiroAPIRegion');
            if (regionEl) body.region = regionEl.value;
            const regionsEl = document.getElementById('kiroAPIRegions');
            if (regionsEl) {
                const raw = (regionsEl.value || '').trim();
                body.regions = raw === '' ? [] : raw.split(',').map(s => s.trim()).filter(s => s.length > 0);
            }
            const stratEl = document.getElementById('poolStrategy');
            if (stratEl) body.poolStrategy = stratEl.value;
            const fastConcEl = document.getElementById('poolFastConcurrency');
            if (fastConcEl) {
                const raw = (fastConcEl.value || '').trim();
                body.poolFastConcurrency = raw === '' ? 0 : parseInt(raw, 10);
            }
            const initConcEl = document.getElementById('poolInitialConcurrency');
            if (initConcEl) {
                const raw = (initConcEl.value || '').trim();
                body.poolInitialConcurrency = raw === '' ? 0 : parseInt(raw, 10);
            }
            const maxConcEl = document.getElementById('poolMaxConcurrency');
            if (maxConcEl) {
                const raw = (maxConcEl.value || '').trim();
                body.poolMaxConcurrency = raw === '' ? 0 : parseInt(raw, 10);
            }
            const res = await fetch('/admin/api/endpoint', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify(body)
            });
            const d = await res.json();
            if (d.success) { alert(t('settings.endpointSaved')); } else { alert(t('common.saveFailed') + ': ' + d.error); }
        }
        async function loadProxyConfig() {
            const res = await fetch('/admin/api/proxy', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            const proxyURL = d.proxyURL || '';
            if (!proxyURL) {
                document.getElementById('proxyType').value = 'none';
                document.getElementById('proxyFields').style.display = 'none';
                return;
            }
            try {
                const u = new URL(proxyURL);
                const scheme = u.protocol.replace(':', '');
                document.getElementById('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
                document.getElementById('proxyHost').value = u.hostname;
                document.getElementById('proxyPort').value = u.port;
                document.getElementById('proxyUsername').value = decodeURIComponent(u.username);
                document.getElementById('proxyPassword').value = decodeURIComponent(u.password);
                document.getElementById('proxyFields').style.display = '';
            } catch(e) {
                document.getElementById('proxyType').value = 'none';
                document.getElementById('proxyFields').style.display = 'none';
            }
        }
        function onProxyTypeChange() {
            const type = document.getElementById('proxyType').value;
            document.getElementById('proxyFields').style.display = type === 'none' ? 'none' : '';
        }
        async function saveProxyConfig() {
            const type = document.getElementById('proxyType').value;
            let proxyURL = '';
            if (type !== 'none') {
                const host = document.getElementById('proxyHost').value.trim();
                const port = document.getElementById('proxyPort').value.trim();
                if (!host || !port) { alert(t('settings.proxyHostRequired')); return; }
                const user = document.getElementById('proxyUsername').value.trim();
                const pass = document.getElementById('proxyPassword').value.trim();
                const auth = user ? (pass ? `${encodeURIComponent(user)}:${encodeURIComponent(pass)}@` : `${encodeURIComponent(user)}@`) : '';
                proxyURL = `${type}://${auth}${host}:${port}`;
            }
            const res = await fetch('/admin/api/proxy', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ proxyURL })
            });
            const d = await res.json();
            if (d.success) { alert(t('settings.proxySaved')); } else { alert(t('common.saveFailed') + ': ' + d.error); }
        }
        var promptRules = [];
        async function loadPromptFilter() {
            const res = await fetch('/admin/api/prompt-filter', { headers: { 'X-Admin-Password': password } });
            const d = await res.json();
            document.getElementById('filterClaudeCode').checked = !!d.filterClaudeCode;
            document.getElementById('filterEnvNoise').checked = !!d.filterEnvNoise;
            document.getElementById('filterStripBoundaries').checked = !!d.filterStripBoundaries;
            promptRules = d.rules || [];
            renderPromptRules();
        }
        async function savePromptFilter() {
            const res = await fetch('/admin/api/prompt-filter', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({
                    filterClaudeCode: document.getElementById('filterClaudeCode').checked,
                    filterEnvNoise: document.getElementById('filterEnvNoise').checked,
                    filterStripBoundaries: document.getElementById('filterStripBoundaries').checked,
                    rules: promptRules
                })
            });
            const d = await res.json();
            if (d.success) { alert(t('settings.promptFilterSaved')); } else { alert(t('common.saveFailed') + ': ' + d.error); }
        }
        function renderPromptRules() {
            const container = document.getElementById('promptFilterRules');
            if (!promptRules.length) { container.innerHTML = '<small style="color:#94a3b8">' + t('promptFilter.noRules') + '</small>'; return; }
            container.innerHTML = promptRules.map((r, i) => {
                const typeLabel = r.type === 'lines-containing' ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
                const replacePlaceholder = r.type === 'lines-containing' ? '' : t('promptFilter.emptyRemove');
                const replaceRow = r.type !== 'lines-containing'
                    ? '<div style="margin-top:4px"><b>' + t('promptFilter.replace') + ':</b> <input value="' + escapeHTML(r.replace||'') + '" onchange="updateRule(' + i + ',\'replace\',this.value)" style="width:70%;padding:2px 6px;border:1px solid #d1d5db;border-radius:3px;font-size:12px;font-family:monospace" placeholder="' + replacePlaceholder + '"></div>'
                    : '';
                return '<div style="border:1px solid #e2e8f0;border-radius:6px;padding:8px 12px;margin-bottom:6px;background:' + (r.enabled?'#f0fdf4':'#fef2f2') + '">' +
                    '<div style="display:flex;align-items:center;gap:8px;margin-bottom:4px">' +
                    '<label class="switch" style="transform:scale(0.8)"><input type="checkbox" ' + (r.enabled?'checked':'') + ' onchange="toggleRule(' + i + ')"><span class="slider"></span></label>' +
                    '<input value="' + escapeHTML(r.name||'') + '" onchange="updateRule(' + i + ',\'name\',this.value)" style="font-size:13px;font-weight:600;border:none;border-bottom:1px dashed #cbd5e1;background:transparent;outline:none;flex:1" placeholder="' + t('promptFilter.unnamed') + '">' +
                    '<span style="font-size:11px;color:#64748b;background:#f1f5f9;padding:2px 6px;border-radius:3px;white-space:nowrap">' + typeLabel + '</span>' +
                    '<button onclick="removeRule(' + i + ')" style="margin-left:4px;background:none;border:none;color:#ef4444;cursor:pointer;font-size:16px;line-height:1">&times;</button>' +
                    '</div>' +
                    '<div style="font-size:12px;color:#475569">' +
                    '<div><b>' + t('promptFilter.match') + ':</b> <input value="' + escapeHTML(r.match||'') + '" onchange="updateRule(' + i + ',\'match\',this.value)" style="width:70%;padding:2px 6px;border:1px solid #d1d5db;border-radius:3px;font-size:12px;font-family:monospace"></div>' +
                    replaceRow +
                    '</div>' +
                    '</div>';
            }).join('');
        }
        function addPromptRule(type) {
            const id = 'rule-' + Date.now();
            promptRules.push({ id, name: '', type, match: '', replace: '', enabled: true });
            renderPromptRules();
        }
        function removeRule(i) { promptRules.splice(i, 1); renderPromptRules(); }
        function toggleRule(i) { promptRules[i].enabled = !promptRules[i].enabled; renderPromptRules(); }
        function updateRule(i, field, value) { promptRules[i][field] = value; }
        function generateApiKey() {
            const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
            let key = 'sk-';
            for (let i = 0; i < 32; i++) key += chars.charAt(Math.floor(Math.random() * chars.length));
            document.getElementById('apiKeyInput').value = key;
        }
        async function saveSettings() {
            const requireApiKey = document.getElementById('requireApiKey').checked;
            const apiKeyInput = document.getElementById('apiKeyInput');
            if (requireApiKey && !apiKeyInput.value.trim()) {
                generateApiKey();
            }
            await fetch('/admin/api/settings', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ requireApiKey, apiKey: apiKeyInput.value })
            });
            alert(t('detail.saved'));
        }
        async function saveOverUsageConfig() {
            const allowOverUsage = document.getElementById('allowOverUsage').checked;
            await fetch('/admin/api/settings', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ allowOverUsage })
            });
            alert(t('settings.overUsageSaved'));
        }
        async function changePassword() {
            const newPwd = document.getElementById('newPassword').value;
            if (!newPwd) return alert(t('settings.passwordRequired'));
            await fetch('/admin/api/settings', {
                method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ password: newPwd })
            });
            password = newPwd;
            localStorage.setItem('admin_password', password);
            alert(t('settings.passwordChanged'));
            document.getElementById('newPassword').value = '';
        }
        async function resetStats() {
            if (!await uiConfirm(t('settings.confirmReset'), { danger: true })) return;
            await fetch('/admin/api/stats/reset', { method: 'POST', headers: { 'X-Admin-Password': password } });
            loadStats();
        }
        async function toggleAccount(id, enabled) {
            await fetch('/admin/api/accounts/' + id, {
                method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ enabled })
            });
            loadAccounts();
        }
        // toggleOverage flips a single account's AllowOverage flag in one click.
        // When on, the account keeps serving after its quota is exhausted
        // (at OverageWeight frequency). Optimistically updates the local row
        // so the button repaints instantly, then reloads to reflect the
        // server's canonical state.
        async function toggleOverage(id, allow) {
            const acc = accountsData.find(a => a.id === id);
            if (acc) acc.allowOverage = allow;
            renderAccounts();
            try {
                await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ allowOverage: allow })
                });
            } catch (e) { /* fall through to reload */ }
            loadAccounts();
        }
        // refreshAwsOverage re-reads the REAL AWS Overages switch + live billing
        // $ for one account and re-opens the detail panel with fresh values.
        async function refreshAwsOverage(id) {
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/overage', {
                    headers: { 'X-Admin-Password': password }
                });
                const d = await res.json();
                if (!res.ok) { alert((d && d.error) || t('common.saveFailed')); return; }
                await loadAccounts();
                showDetail(id);
            } catch (e) { alert(t('common.saveFailed') + ': ' + e); }
        }
        // setAwsOverage flips the REAL AWS billing switch. Enabling authorizes
        // real overage charges, so it requires an explicit confirm both in the
        // UI and via the confirm=true flag the backend enforces.
        async function setAwsOverage(id, enabled) {
            if (enabled && !await uiConfirm(t('detail.awsOverageConfirm'))) return;
            try {
                const res = await fetch('/admin/api/accounts/' + id + '/overage', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ enabled: enabled, confirm: true })
                });
                const d = await res.json();
                if (!res.ok) { alert((d && d.error) || t('common.saveFailed')); return; }
                await loadAccounts();
                showDetail(id);
            } catch (e) { alert(t('common.saveFailed') + ': ' + e); }
        }
        async function deleteAccount(id) {
            if (!await uiConfirm(t('accounts.confirmDelete'), { danger: true })) return;
            await fetch('/admin/api/accounts/' + id, { method: 'DELETE', headers: { 'X-Admin-Password': password } });
            loadAccounts(); loadStats();
        }
        function formatNum(n) {
            if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
            if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
            return n.toString();
        }
        // fmtCooldown renders remaining cooldown seconds compactly: 45s, 3m, 1h12m.
        function fmtCooldown(secs) {
            secs = Math.max(0, Math.round(secs || 0));
            if (secs < 60) return secs + 's';
            if (secs < 3600) {
                const m = Math.floor(secs / 60), s = secs % 60;
                return s ? m + 'm' + s + 's' : m + 'm';
            }
            const h = Math.floor(secs / 3600), m = Math.floor((secs % 3600) / 60);
            return m ? h + 'h' + m + 'm' : h + 'h';
        }
        function copy(id) {
            navigator.clipboard.writeText(document.getElementById(id).textContent);
            alert(t('common.copied'));
        }
        // copyApiKey fetches the full secret for one API key and copies it
        // to the clipboard. The list endpoint returns only masked keys
        // (sk-kg-...wxyz) so a screenshare can't leak secrets; this button
        // gives operators a one-click reveal-and-copy without storing the
        // full secret in the rendered DOM.
        async function copyApiKey(id) {
            try {
                const res = await fetch('/admin/api/apikeys/' + encodeURIComponent(id) + '/reveal', {
                    headers: { 'X-Admin-Password': password }
                });
                if (!res.ok) {
                    alert(t('apikeys.copyFailed') || 'Reveal failed: ' + res.status);
                    return;
                }
                const data = await res.json();
                if (!data.key) {
                    alert(t('apikeys.copyFailed') || 'No key in response');
                    return;
                }
                copyValue(data.key);
            } catch (e) {
                alert(t('apikeys.copyFailed') || ('Copy failed: ' + e.message));
            }
        }
        // copyValue copies a literal string instead of an element's text. Used
        // by the API-key list (and elsewhere) where the value is rendered
        // inside an attribute rather than as an addressable element id.
        function copyValue(s) {
            try {
                navigator.clipboard.writeText(s);
                alert(t('common.copied'));
            } catch (e) {
                // Older browsers / non-secure contexts fall through to a
                // textarea-based fallback so the button still works on
                // self-hosted HTTP installs.
                const ta = document.createElement('textarea');
                ta.value = s;
                document.body.appendChild(ta);
                ta.select();
                try { document.execCommand('copy'); alert(t('common.copied')); }
                finally { ta.remove(); }
            }
        }

        async function copyAccountJSON(accountId, buttonElement) {
            try {
                // 从后端获取完整账号信息（包含敏感字段）
                const res = await fetch('/admin/api/accounts/' + accountId + '/full', {
                    headers: { 'X-Admin-Password': password }
                });

                if (!res.ok) {
                    throw new Error('Failed to fetch account data');
                }

                const account = await res.json();
                const { clientId, clientSecret, accessToken, refreshToken } = account;
                const jsonString = JSON.stringify({ clientId, clientSecret, accessToken, refreshToken }, null, 2);

                if (navigator.clipboard && navigator.clipboard.writeText) {
                    await navigator.clipboard.writeText(jsonString);
                } else {
                    const textarea = document.createElement('textarea');
                    textarea.value = jsonString;
                    textarea.style.position = 'fixed';
                    textarea.style.opacity = '0';
                    document.body.appendChild(textarea);
                    textarea.select();
                    const success = document.execCommand('copy');
                    document.body.removeChild(textarea);
                    if (!success) throw new Error('execCommand failed');
                }

                showCopySuccess(buttonElement);
                alert(t('accounts.copyJSONSuccess'));
            } catch (error) {
                console.error('Copy failed:', error);
                alert(t('common.failed'));
            }
        }

        function showCopySuccess(buttonElement) {
            const originalHTML = buttonElement.innerHTML;
            const originalClass = buttonElement.className;

            buttonElement.disabled = true;
            buttonElement.className = 'btn btn-sm btn-icon btn-success';
            buttonElement.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="20 6 9 17 4 12"></polyline></svg>';

            setTimeout(() => {
                buttonElement.disabled = false;
                buttonElement.className = originalClass;
                buttonElement.innerHTML = originalHTML;
            }, 800);
        }

        function showModal(type) {
            const modal = document.getElementById('addModal');
            const title = document.getElementById('modalTitle');
            const body = document.getElementById('modalBody');
            if (type === 'add') {
                title.textContent = t('modal.addAccount');
                body.innerHTML =
                    '<div style="display:flex;flex-direction:column;gap:12px">' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'builderid\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.builderIdTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.builderIdDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'iam\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.iamTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.iamDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'sso\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.ssoTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.ssoDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'local\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.localTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.localDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'credentials\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.credentialsTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.credentialsDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'cookie\')" onmouseover="this.style.borderColor=\'#7c3aed\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">' + t('modal.cookieTitle') + '</div><div style="font-size:13px;color:#64748b">' + t('modal.cookieDesc') + '</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'codex\')" onmouseover="this.style.borderColor=\'#3b82f6\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">OpenAI Codex (ChatGPT)</div><div style="font-size:13px;color:#64748b">Connect a ChatGPT/Codex account via OpenAI login, or paste an access token.</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'qoder\')" onmouseover="this.style.borderColor=\'#ec4899\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">Qoder</div><div style="font-size:13px;color:#64748b">Connect a Qoder account via device login (opens qoder.com in your browser).</div></div>' +
                    '<div class="card" style="margin:0;cursor:pointer;border:2px solid transparent;transition:border-color 0.2s" onclick="showModal(\'qwen\')" onmouseover="this.style.borderColor=\'#9333ea\'" onmouseout="this.style.borderColor=\'transparent\'"><div style="font-weight:600;margin-bottom:6px">Qwen (Alibaba)</div><div style="font-size:13px;color:#64748b">Connect a Qwen account via device login (qwen-code OAuth) — shows a code to enter at chat.qwen.ai.</div></div>' +
                    '</div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="closeModal()">' + t('common.cancel') + '</button></div>';
            } else if (type === 'builderid') {
                title.textContent = t('modal.builderIdTitle');
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">' + t('modal.builderIdDesc') + '</p>' +
                    '<div id="builderIdStep1"><div class="form-group"><label>Region</label><input type="text" id="builderIdRegion" value="us-east-1"></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="startBuilderIdLogin()">' + t('builderid.startLogin') + '</button></div></div>' +
                    '<div id="builderIdStep2" class="hidden">' +
                    '<div class="message" style="background:#ede9fe;color:#7c3aed;text-align:center"><p style="font-size:18px;font-weight:600;margin-bottom:8px" id="builderIdUserCode"></p><p style="font-size:12px">' + t('builderid.verifyCode') + '</p></div>' +
                    '<div class="form-group" style="margin-top:16px"><label>' + t('builderid.verifyUrl') + '</label><div class="endpoint" style="margin-bottom:0"><span id="builderIdVerifyUrl" style="font-size:12px"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'builderIdVerifyUrl\').textContent,\'_blank\')">' + t('builderid.open') + '</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'builderIdVerifyUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<p id="builderIdStatus" style="color:#64748b;margin:16px 0;font-size:13px;text-align:center">' + t('builderid.waiting') + '</p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelBuilderIdLogin()">' + t('common.cancel') + '</button></div>' +
                    '</div>';
            } else if (type === 'local') {
                title.textContent = t('modal.localTitle');
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">' + t('modal.localDesc') + '</p>' +
                    '<div style="font-size:13px;color:#64748b;margin-bottom:16px;line-height:1.6"><p style="margin-bottom:8px"><b>' + t('local.fileLocation') + '</b></p><p style="margin-bottom:4px">Windows: <code style="background:#f1f5f9;padding:2px 6px;border-radius:4px;font-size:11px">%USERPROFILE%\\.aws\\sso\\cache\\</code></p><p>macOS/Linux: <code style="background:#f1f5f9;padding:2px 6px;border-radius:4px;font-size:11px">~/.aws/sso/cache/</code></p></div>' +
                    '<div class="form-group"><label>' + t('local.loginChannel') + '</label><select id="localProvider" onchange="updateLocalFields()"><option value="BuilderId">AWS Builder ID</option><option value="Enterprise">IAM Identity Center (Enterprise SSO)</option><option value="Google">Google</option><option value="Github">GitHub</option></select></div>' +
                    '<div class="form-group"><label>' + t('local.tokenFile') + ' <span style="font-weight:normal;color:#64748b;font-size:12px">' + t('local.tokenRequired') + '</span></label><div style="display:flex;gap:8px;align-items:stretch"><textarea id="localTokenJson" placeholder="' + t('local.pasteOrUpload') + '" style="flex:1;min-height:80px;font-size:12px"></textarea><label class="btn btn-secondary" style="display:flex;align-items:center;cursor:pointer">' + t('local.upload') + '<input type="file" accept=".json" style="display:none" onchange="loadLocalFile(this,\'localTokenJson\')"></label></div></div>' +
                    '<div id="localClientGroup" class="form-group"><label>' + t('local.clientFile') + ' <span style="font-weight:normal;color:#64748b;font-size:12px">' + t('local.clientRequired') + '</span></label><div style="display:flex;gap:8px;align-items:stretch"><textarea id="localClientJson" placeholder="' + t('local.pasteOrUpload') + '" style="flex:1;min-height:80px;font-size:12px"></textarea><label class="btn btn-secondary" style="display:flex;align-items:center;cursor:pointer">' + t('local.upload') + '<input type="file" accept=".json" style="display:none" onchange="loadLocalFile(this,\'localClientJson\')"></label></div></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="importLocalKiro()">' + t('common.add') + '</button></div>';
            } else if (type === 'credentials') {
                title.textContent = t('modal.credentialsTitle');
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">' + t('modal.credentialsDesc') + '</p>' +
                    '<div style="font-size:12px;color:#64748b;margin-bottom:12px;line-height:1.5">' + t('credentials.batchHint') + '</div>' +
                    '<div class="form-group"><label>' + t('credentials.label') + '</label><textarea id="credJson" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderId"},{"refreshToken":"yyy","clientId":"...","clientSecret":"...","provider":"Enterprise"}]\' style="min-height:120px"></textarea></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="importCredentials()">' + t('common.add') + '</button></div>';
            } else if (type === 'cookie') {
                title.textContent = t('modal.cookieTitle');
                body.innerHTML =
                    '<div style="font-size:13px;color:#64748b;margin-bottom:16px;line-height:1.8"><p style="margin-bottom:8px;font-weight:600;color:#374151">' + t('cookie.howToGet') + '</p><ol style="margin:0;padding-left:20px;display:flex;flex-direction:column;gap:6px"><li>' + t('cookie.step1') + ' <a href="' + t('cookie.link') + '" target="_blank" style="color:#7c3aed;word-break:break-all">' + t('cookie.link') + '</a></li><li>' + t('cookie.step2') + '</li><li>' + t('cookie.step3') + '</li></ol></div>' +
                    '<div class="form-group"><label>' + t('cookie.provider') + '</label><select id="cookieProvider"><option value="Google">' + t('cookie.google') + '</option><option value="Github">' + t('cookie.github') + '</option></select></div>' +
                    '<div class="form-group"><label>' + t('cookie.refreshToken') + '</label><textarea id="cookieRefreshToken" placeholder="' + t('cookie.refreshTokenPlaceholder') + '" style="min-height:80px;font-family:monospace;font-size:12px"></textarea></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="importFromCookie()">' + t('common.add') + '</button></div>';
            } else if (type === 'sso') {
                title.textContent = t('modal.ssoTitle');
                body.innerHTML =
                    '<div style="font-size:13px;color:#64748b;margin-bottom:16px;line-height:1.6"><p style="margin-bottom:8px"><b>' + t('sso.howToGet') + '</b></p><ol style="margin:0;padding-left:20px"><li>' + t('sso.step1') + ' <code style="background:#f1f5f9;padding:2px 6px;border-radius:4px">view.awsapps.com/start</code></li><li>' + t('sso.step2') + '</li><li>' + t('sso.step3') + ' <code style="background:#f1f5f9;padding:2px 6px;border-radius:4px">x-amz-sso_authn</code></li></ol></div>' +
                    '<div class="form-group"><label>' + t('sso.tokenLabel') + ' <span style="font-weight:normal;color:#64748b;font-size:12px">' + t('sso.tokenHint') + '</span></label><textarea id="ssoToken" placeholder="' + t('sso.tokenPlaceholder') + '" style="min-height:120px"></textarea></div>' +
                    '<div class="form-group"><label>Region</label><input type="text" id="ssoRegion" value="us-east-1"></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="importSsoToken()">' + t('common.add') + '</button></div>';
            } else if (type === 'iam') {
                title.textContent = t('modal.iamTitle');
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">' + t('modal.iamDesc') + '</p>' +
                    '<div class="form-group"><label>' + t('iam.startUrl') + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start"></div>' +
                    '<div class="form-group"><label>Region</label><input type="text" id="iamRegion" value="us-east-1"></div>' +
                    '<div id="iamStep2" class="hidden"><div class="form-group"><label>' + t('iam.loginUrl') + '</label><div class="endpoint" style="margin-bottom:0"><span id="iamAuthUrl" style="font-size:11px"></span></div><div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'iamAuthUrl\').textContent,\'_blank\')">' + t('builderid.open') + '</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'iamAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div><p style="color:#16a34a;margin:12px 0;font-size:14px">' + t('iam.completeLogin') + '</p><div class="form-group"><label>' + t('iam.callbackUrl') + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..."></div></div>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" id="iamBtn" onclick="startIamSso()">' + t('builderid.startLogin') + '</button></div>';
            } else if (type === 'codex') {
                title.textContent = 'OpenAI Codex (ChatGPT)';
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">Connect a ChatGPT/Codex account. Browser login uses OpenAI on the fixed callback port 1455 (the same port the Codex CLI uses — make sure no Codex CLI login is running). Or paste an access/id token directly.</p>' +
                    '<div id="codexStep1">' +
                    '<div class="modal-footer" style="justify-content:flex-start;gap:8px"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="startCodexLogin()">Login with OpenAI</button></div>' +
                    '<div style="margin:16px 0;text-align:center;color:#94a3b8;font-size:12px">— or —</div>' +
                    '<div class="form-group"><label>Paste Access / ID Token <span style="font-weight:normal;color:#64748b;font-size:12px">(optional)</span></label><textarea id="codexToken" placeholder="eyJ..." style="min-height:80px;font-family:monospace;font-size:12px"></textarea></div>' +
                    '<div class="form-group"><label>Name <span style="font-weight:normal;color:#64748b;font-size:12px">(optional)</span></label><input type="text" id="codexName" placeholder="My ChatGPT account"></div>' +
                    '<div class="modal-footer"><button class="btn btn-primary" onclick="importCodexToken()">' + t('common.add') + ' (token)</button></div>' +
                    '</div>' +
                    '<div id="codexStep2" class="hidden">' +
                    '<div class="form-group"><label>Login URL</label><div class="endpoint" style="margin-bottom:0"><span id="codexAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'codexAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'codexAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<p id="codexStatus" style="color:#64748b;margin:16px 0;font-size:13px;text-align:center">Waiting for you to authorize in the browser…</p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelCodexLogin()">' + t('common.cancel') + '</button></div>' +
                    '</div>';
            } else if (type === 'qoder') {
                title.textContent = 'Qoder';
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">Connect a Qoder account. Clicking "Login with Qoder" opens qoder.com in your browser; authorize there and this dialog completes automatically. Device tokens last ~30 days (re-login when expired).</p>' +
                    '<div id="qoderStep1"><div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="startQoderLogin()">Login with Qoder</button></div></div>' +
                    '<div id="qoderStep2" class="hidden">' +
                    '<div class="form-group"><label>Login URL</label><div class="endpoint" style="margin-bottom:0"><span id="qoderAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'qoderAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'qoderAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<p id="qoderStatus" style="color:#64748b;margin:16px 0;font-size:13px;text-align:center">Waiting for you to authorize in the browser…</p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelQoderLogin()">' + t('common.cancel') + '</button></div>' +
                    '</div>';
            } else if (type === 'qwen') {
                title.textContent = 'Qwen (Alibaba)';
                body.innerHTML =
                    '<p style="font-size:13px;color:#64748b;margin-bottom:16px">Connect a Qwen account via the qwen-code OAuth device flow. Clicking "Login with Qwen" shows a code to enter at chat.qwen.ai; authorize there and this dialog completes automatically. Access tokens auto-refresh.</p>' +
                    '<div id="qwenStep1"><div class="modal-footer"><button class="btn btn-secondary" onclick="showModal(\'add\')">' + t('common.back') + '</button><button class="btn btn-primary" onclick="startQwenLogin()">Login with Qwen</button></div></div>' +
                    '<div id="qwenStep2" class="hidden">' +
                    '<div class="message" style="background:#f3e8ff;color:#9333ea;text-align:center"><p style="font-size:12px;margin-bottom:6px">Enter this code at the verification page:</p><p style="font-size:22px;font-weight:700;letter-spacing:2px" id="qwenUserCode"></p></div>' +
                    '<div class="form-group" style="margin-top:12px"><label>Verification URL</label><div class="endpoint" style="margin-bottom:0"><span id="qwenAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'qwenAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'qwenAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<p id="qwenStatus" style="color:#64748b;margin:16px 0;font-size:13px;text-align:center">Waiting for you to authorize in the browser…</p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelQwenLogin()">' + t('common.cancel') + '</button></div>' +
                    '</div>';
            }
            modal.classList.add('active');
        }
        function closeModal() {
            document.getElementById('addModal').classList.remove('active');
            iamSession = '';
            if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
            builderIdSession = '';
            if (typeof codexPollTimer !== 'undefined' && codexPollTimer) { clearTimeout(codexPollTimer); codexPollTimer = null; }
            if (typeof codexSessionId !== 'undefined') { codexSessionId = ''; }
            if (typeof qoderPollTimer !== 'undefined' && qoderPollTimer) { clearTimeout(qoderPollTimer); qoderPollTimer = null; }
            if (typeof qoderSessionId !== 'undefined') { qoderSessionId = ''; }
            if (typeof qwenPollTimer !== 'undefined' && qwenPollTimer) { clearTimeout(qwenPollTimer); qwenPollTimer = null; }
            if (typeof qwenSessionId !== 'undefined') { qwenSessionId = ''; }
            if (typeof oauthFlowTimer !== 'undefined' && oauthFlowTimer) { clearTimeout(oauthFlowTimer); oauthFlowTimer = null; }
            if (typeof oauthFlowSession !== 'undefined') { oauthFlowSession = ''; }
            if (typeof oauthFlowId !== 'undefined') { oauthFlowId = ''; }
        }
        // Add a non-Kiro provider account. Two shapes:
        //   - a named built-in provider (Groq, OpenAI, DeepSeek, ...) — just an
        //     API key; base URL and dialect are known.
        //   - "Custom (OpenAI/Anthropic/Gemini-compatible)" — paste an API BASE
        //     URL (e.g. https://api.example.com/v1) + key; we derive
        //     /chat/completions and /models from it and fetch the model list on
        //     add. This is NOT the Kiro account schema.
        async function showAddProviderModal() {
            const modal = document.getElementById('addModal');
            const title = document.getElementById('modalTitle');
            const body = document.getElementById('modalBody');
            title.textContent = 'Add Provider Account';
            body.innerHTML = '<p style="font-size:13px;color:#64748b;margin-bottom:12px">Loading providers…</p>';
            modal.classList.add('active');
            let cat = [];
            let oauthCat = [];
            try {
                const res = await fetch('/admin/api/providers/catalog', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                const all = d.providers || [];
                cat = all.filter(p => p.authType === 'apikey');
                // Any catalog provider with a wired connect flow gets a login/import
                // button — including dual-mode providers (e.g. xai) that also accept a
                // pasted API key and so still appear in the dropdown above.
                oauthCat = all.filter(p => OAUTH_FLOWS[p.id]);
            } catch (e) { /* fall through to empty */ }
            const builtinOpts = cat.map(p => '<option value="' + p.id + '" data-dialect="' + p.dialect + '">' + (p.name || p.id) + ' (' + p.dialect + ')</option>').join('');
            const opts = builtinOpts + '<option value="custom">➕ Custom (OpenAI / Anthropic / Gemini compatible)</option>';
            // Browser/OAuth/import providers get a button grid — each opens its own connect flow.
            let oauthSection = '';
            if (oauthCat.length) {
                const btns = oauthCat.map(p => {
                    const f = OAUTH_FLOWS[p.id];
                    return '<button class="btn btn-secondary" style="justify-content:flex-start;text-align:left" onclick="openOAuthProvider(\'' + p.id + '\')">' +
                        '<span style="font-weight:600">' + (p.name || p.id) + '</span>' +
                        '<span style="font-size:11px;color:var(--muted);margin-left:6px">' + (f.kindLabel || '') + '</span>' +
                        '</button>';
                }).join('');
                oauthSection =
                    '<div style="margin-bottom:18px">' +
                    '<div style="font-size:12px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.4px;margin-bottom:8px">Browser login / token import</div>' +
                    '<div style="display:grid;grid-template-columns:1fr 1fr;gap:8px">' + btns + '</div>' +
                    '<div style="font-size:12px;color:var(--muted);margin-top:8px">These providers authenticate via a browser login, device code, or imported token instead of an API key.</div>' +
                    '</div>' +
                    '<div style="border-top:1px solid var(--border);margin-bottom:16px"></div>';
            }
            body.innerHTML =
                oauthSection +
                '<p style="font-size:13px;color:#64748b;margin-bottom:16px">Add any API-key provider. Models route with a <code style="background:#f1f5f9;padding:1px 5px;border-radius:4px">provider/model</code> prefix (e.g. <code style="background:#f1f5f9;padding:1px 5px;border-radius:4px">groq/llama-3.3-70b</code>). Pick <b>Custom</b> to bring your own OpenAI-compatible endpoint by base URL — its models are fetched automatically.</p>' +
                '<div class="form-group"><label>Provider</label><select id="provBackend" onchange="onProvBackendChange()">' + opts + '</select></div>' +
                '<div id="provCustomFields" class="hidden">' +
                '  <div class="form-group"><label>Display Name</label><input type="text" id="provCustomName" placeholder="My LLM Gateway"></div>' +
                '  <div class="form-group"><label>API Base URL <span style="font-weight:normal;color:#64748b;font-size:12px">(not the /chat/completions URL — just the base)</span></label><input type="text" id="provCustomBase" placeholder="https://api.example.com/v1"></div>' +
                '  <div class="form-group"><label>Dialect</label><select id="provCustomDialect"><option value="openai">OpenAI-compatible</option><option value="anthropic">Anthropic-compatible</option><option value="gemini">Gemini</option></select></div>' +
                '  <div class="form-group"><label>Route Prefix <span style="font-weight:normal;color:#64748b;font-size:12px">(optional; defaults to a slug of the name)</span></label><input type="text" id="provCustomAlias" placeholder="mygw"></div>' +
                '  <div class="form-group"><label>Models <span style="font-weight:normal;color:#64748b;font-size:12px">(optional; comma-separated. Use this if the endpoint has no /models list)</span></label><input type="text" id="provCustomModels" placeholder="gpt-4o, llama-3.3-70b"></div>' +
                '</div>' +
                '<div class="form-group" style="display:flex;align-items:center;gap:8px"><label class="switch" style="margin:0"><input type="checkbox" id="provBulk" onchange="onProvBulkChange()"><span class="slider"></span></label><span style="font-size:13px">Bulk add — paste many API keys (one per line)</span></div>' +
                '<div class="form-group" id="provApiKeyRow"><label>API Key</label><input type="password" id="provApiKey" placeholder="sk-..."></div>' +
                '<div class="form-group hidden" id="provApiKeysRow"><label>API Keys <span style="font-weight:normal;color:#64748b;font-size:12px">(one per line — each becomes its own account on this provider)</span></label><textarea id="provApiKeys" rows="8" oninput="updateProvBulkCount()" style="width:100%;font-family:ui-monospace,Menlo,monospace;font-size:12px;resize:vertical" placeholder="sk-key-one&#10;sk-key-two&#10;sk-key-three"></textarea><div id="provBulkCount" style="font-size:12px;color:#64748b;margin-top:4px"></div></div>' +
                '<div class="form-group" id="provNameRow"><label>Name <span style="font-weight:normal;color:#64748b;font-size:12px">(optional)</span></label><input type="text" id="provName" placeholder="My Groq key"></div>' +
                '<div class="form-group"><label>Token limit per key <span style="font-weight:normal;color:#64748b;font-size:12px">(optional; 0 = unlimited. Each key is dropped from rotation once it hits this many total tokens, so traffic stacks onto keys that still have budget)</span></label><input type="number" id="provTokenLimit" min="0" step="1000" placeholder="e.g. 1000000"></div>' +
                '<div id="provAddStatus" style="font-size:13px;color:#64748b;margin:8px 0;min-height:18px"></div>' +
                '<div class="modal-footer"><button class="btn btn-secondary" onclick="closeModal()">' + t('common.cancel') + '</button><button class="btn btn-primary" id="provAddBtn" onclick="submitProviderAccount()">' + t('common.add') + '</button></div>';
            onProvBackendChange();
        }
        function onProvBackendChange() {
            const sel = document.getElementById('provBackend');
            if (!sel) return;
            const isCustom = sel.value === 'custom';
            const custom = document.getElementById('provCustomFields');
            const nameRow = document.getElementById('provNameRow');
            if (custom) custom.classList.toggle('hidden', !isCustom);
            // The generic "Name" field is redundant for custom (it has its own
            // Display Name), so hide it there.
            if (nameRow) nameRow.classList.toggle('hidden', isCustom);
        }
        // Bulk toggle: swap the single password input for a multi-line textarea so
        // the operator can paste many keys (one per line) — each becomes its own
        // account on the selected provider.
        function onProvBulkChange() {
            const bulk = document.getElementById('provBulk');
            const on = !!(bulk && bulk.checked);
            const single = document.getElementById('provApiKeyRow');
            const many = document.getElementById('provApiKeysRow');
            if (single) single.classList.toggle('hidden', on);
            if (many) many.classList.toggle('hidden', !on);
            if (on) updateProvBulkCount();
        }
        function updateProvBulkCount() {
            const ta = document.getElementById('provApiKeys');
            const out = document.getElementById('provBulkCount');
            if (!ta || !out) return;
            const seen = {};
            let n = 0;
            ta.value.split(/\r?\n/).forEach(line => {
                const k = line.trim();
                if (k && !seen[k]) { seen[k] = true; n++; }
            });
            out.textContent = n === 0 ? '' : (n + ' unique key' + (n === 1 ? '' : 's') + ' will be added');
        }
        async function submitProviderAccount() {
            const backend = document.getElementById('provBackend').value;
            const bulkEl = document.getElementById('provBulk');
            const bulk = !!(bulkEl && bulkEl.checked);
            if (!backend) { alert('Pick a provider'); return; }

            // Collect either one key or the de-duped textarea lines.
            let apiKey = '';
            let apiKeys = [];
            if (bulk) {
                const seen = {};
                document.getElementById('provApiKeys').value.split(/\r?\n/).forEach(line => {
                    const k = line.trim();
                    if (k && !seen[k]) { seen[k] = true; apiKeys.push(k); }
                });
                if (apiKeys.length === 0) { alert('Paste at least one API key (one per line)'); return; }
            } else {
                apiKey = document.getElementById('provApiKey').value.trim();
                if (!apiKey) { alert('API key is required'); return; }
            }

            const payload = { backend };
            if (bulk) { payload.apiKeys = apiKeys; } else { payload.apiKey = apiKey; }
            const tokLim = parseInt(document.getElementById('provTokenLimit').value, 10);
            if (!isNaN(tokLim) && tokLim > 0) payload.tokenLimit = tokLim;
            if (backend === 'custom') {
                payload.name = document.getElementById('provCustomName').value.trim();
                payload.baseURL = document.getElementById('provCustomBase').value.trim();
                payload.dialect = document.getElementById('provCustomDialect').value;
                payload.alias = document.getElementById('provCustomAlias').value.trim();
                const modelsRaw = (document.getElementById('provCustomModels').value || '').trim();
                if (modelsRaw) {
                    payload.models = modelsRaw.split(',').map(s => s.trim()).filter(Boolean);
                }
                if (!payload.baseURL) { alert('API Base URL is required for a custom provider'); return; }
            } else {
                payload.name = document.getElementById('provName').value.trim();
            }
            const btn = document.getElementById('provAddBtn');
            const statusEl = document.getElementById('provAddStatus');
            if (btn) { btn.disabled = true; btn.textContent = 'Adding…'; }
            const url = bulk ? '/admin/api/providers/account/bulk' : '/admin/api/providers/account';
            if (statusEl) statusEl.textContent = bulk
                ? ('Adding ' + apiKeys.length + ' accounts and fetching models…')
                : 'Adding account and fetching models…';
            try {
                const res = await fetch(url, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify(payload)
                });
                const d = await res.json();
                if (d.success) {
                    if (statusEl) statusEl.textContent = bulk
                        ? ('Added ' + (d.added || 0) + ' accounts' + (d.skipped ? ' (' + d.skipped + ' duplicates skipped)' : '') + ' — ' + (d.modelCount || 0) + ' models found.')
                        : ('Added — ' + (d.modelCount || 0) + ' models found.');
                    setTimeout(() => { closeModal(); loadAccounts(); loadProviders(); }, 800);
                } else {
                    if (btn) { btn.disabled = false; btn.textContent = t('common.add'); }
                    if (statusEl) statusEl.textContent = '';
                    alert(d.error || 'Failed to add provider account');
                }
            } catch (e) {
                if (btn) { btn.disabled = false; btn.textContent = t('common.add'); }
                if (statusEl) statusEl.textContent = '';
                alert('Failed to add provider account');
            }
        }
        // ---- Providers tab ----
        // Renders non-Kiro provider accounts grouped by backend, each showing its
        // dialect, account/key status, and fetched model count, with per-account
        // refresh-models / test / delete. Kiro accounts stay on the Accounts tab.
        function loadProviders() {
            // accountsData is the single source of truth (already polled). If it
            // isn't loaded yet, fetch it first, then render.
            if (!accountsData || accountsData.length === 0) {
                loadAccounts().then(renderProviders);
            } else {
                renderProviders();
            }
        }
        const PROVIDER_LABELS = {
            codex: 'OpenAI Codex (ChatGPT)', qoder: 'Qoder', openai: 'OpenAI', anthropic: 'Anthropic',
            gemini: 'Gemini', groq: 'Groq', cerebras: 'Cerebras', deepseek: 'DeepSeek', mistral: 'Mistral',
            openrouter: 'OpenRouter', xai: 'xAI', together: 'Together', fireworks: 'Fireworks',
            cohere: 'Cohere', nebius: 'Nebius', siliconflow: 'SiliconFlow',
            qwen: 'Qwen (Alibaba)', alicode: 'Alibaba Code', 'alicode-intl': 'Alibaba Code (Intl)',
            dashscope: 'Alibaba Model Studio (China)', 'dashscope-intl': 'Alibaba Model Studio (Intl)',
            'dashscope-us': 'Alibaba Model Studio (US)'
        };
        function renderProviders() {
            const container = document.getElementById('providersList');
            if (!container) return;
            const provs = providerAccounts();
            if (provs.length === 0) {
                providerSelection.clear();
                updateProviderBatchBar();
                container.innerHTML = '<div class="empty" style="padding:32px"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><rect x="2" y="2" width="8" height="8" rx="1"/><rect x="14" y="2" width="8" height="8" rx="1"/><rect x="2" y="14" width="8" height="8" rx="1"/><path d="M18 14v8M14 18h8"/></svg><div>' + t('providers.empty') + '</div></div>';
                return;
            }
            // Drop any selected ids that no longer exist (e.g. deleted elsewhere) so a
            // stale selection can't survive across the 5s auto-refresh re-render.
            const liveIds = new Set(provs.map(a => a.id));
            providerSelection.forEach(id => { if (!liveIds.has(id)) providerSelection.delete(id); });
            // Group accounts by backend (the provider id).
            const groups = {};
            provs.forEach(a => { const b = (a.backend || '').toLowerCase(); (groups[b] = groups[b] || []).push(a); });

            const html = Object.keys(groups).sort().map(backend => {
                const accts = groups[backend];
                // Card title: a built-in provider uses its catalog label; a custom
                // (bring-your-own) provider uses the operator-set display name
                // (account nickname), NOT the generated routing id — otherwise the
                // header reads the slug/"custom-xxxx" id twice. The routing id still
                // shows in the parenthetical below where it's actually useful.
                let label = PROVIDER_LABELS[backend] || backend;
                const firstAcct = accts[0] || {};
                const isCustomGroup = !PROVIDER_LABELS[backend] && (firstAcct.customDialect || '').trim() !== '';
                if (isCustomGroup) {
                    const nick = (firstAcct.nickname || '').replace(/\s*#\d+\s*$/, '').trim();
                    if (nick) label = nick;
                }
                // Distinct-model count: 100 keys sharing the same 145 models should read
                // as 145 (the provider's actual catalog), not 14,500. We union the per-key
                // model ids; keys with no cached list contribute nothing (and fall back to
                // "any model" in their row badge). When the server only returns a numeric
                // modelCount (older payloads, no modelList), we fall back to the max
                // per-key count rather than the sum — same provider serves the same list.
                const distinctModels = new Set();
                let hasAnyList = false;
                let maxCount = 0;
                accts.forEach(a => {
                    const list = a.modelList || [];
                    if (list.length) { hasAnyList = true; list.forEach(m => distinctModels.add(m)); }
                    if ((a.modelCount || 0) > maxCount) maxCount = a.modelCount || 0;
                });
                const totalModels = hasAnyList ? distinctModels.size : maxCount;
                // Aggregate token usage across every key in this provider card ("stacked"
                // allowance): sum of used tokens, and sum of configured limits (0-limit
                // keys count as unlimited and are excluded from the limit total).
                const usedSum = accts.reduce((s, a) => s + (a.totalTokens || 0), 0);
                const limitSum = accts.reduce((s, a) => s + (a.tokenLimit > 0 ? a.tokenLimit : 0), 0);
                const exhausted = accts.filter(a => a.tokenLimitReached).length;
                // Aggregate upstream quota (CodeBuddy credits / Codex windows) across the
                // card's accounts, so the collapsed header shows quota without expanding.
                const quotaUsedSum = accts.reduce((s, a) => s + (a.usageLimit > 0 ? (a.usageCurrent || 0) : 0), 0);
                const quotaLimitSum = accts.reduce((s, a) => s + (a.usageLimit > 0 ? a.usageLimit : 0), 0);
                const rows = accts.map(a => {
                    const ok = a.hasToken || a.hasApiKey;
                    const statusBadge = ok
                        ? '<span class="badge badge-success">' + t('providers.connected') + '</span>'
                        : '<span class="badge badge-error">' + t('providers.noCreds') + '</span>';
                    const enabledBadge = a.enabled
                        ? '<span class="badge badge-info">' + t('accounts.enabled') + '</span>'
                        : '<span class="badge badge-warning">' + t('accounts.disabled') + '</span>';
                    const modelsBadge = a.modelCount
                        ? '<span class="badge badge-success" title="' + t('providers.modelsHint') + '">' + a.modelCount + ' ' + t('providers.models') + '</span>'
                        : '<span class="badge badge-info" title="' + t('providers.anyModelHint') + '">' + t('providers.anyModel') + '</span>';
                    const planBadge = a.codexPlanType ? '<span class="badge badge-pro">' + a.codexPlanType + '</span>' : '';
                    // Token-limit badge: show used/limit and remaining when a per-key cap
                    // is set; flag an exhausted key (dropped from rotation) in red.
                    let limitBadge = '';
                    if (a.tokenLimit > 0) {
                        const used = a.totalTokens || 0;
                        const remaining = Math.max(0, a.tokenLimit - used);
                        if (a.tokenLimitReached) {
                            limitBadge = '<span class="badge badge-error" title="' + t('providers.tokLimitReachedHint') + '">' + t('providers.tokExhausted') + '</span>';
                        } else {
                            limitBadge = '<span class="badge badge-warning" title="' + formatNum(remaining) + ' ' + t('providers.tokRemaining') + '">' + formatNum(used) + ' / ' + formatNum(a.tokenLimit) + '</span>';
                        }
                    }
                    const name = a.nickname || a.email || a.id.slice(0, 8);
                    // Quota cell: show used / limit + a percentage bar when the upstream
                    // reports a usage allowance (CodeBuddy credits, Codex windows, etc.).
                    // Accounts with no quota signal (usageLimit 0) show a dash.
                    let quotaCell = '<span style="color:var(--muted)">—</span>';
                    if (a.usageLimit > 0) {
                        const pct = Math.max(0, Math.min(100, (a.usagePercent || 0) * 100));
                        const qClass = pct > 90 ? 'critical' : pct > 70 ? 'high' : '';
                        const resetHint = a.nextResetDate ? ' title="resets ' + escapeHtml(String(a.nextResetDate)) + '"' : '';
                        quotaCell = '<div class="mini-usage"' + resetHint + '><div class="usage-bar"><div class="usage-fill ' + qClass + '" style="width:' + pct + '%"></div></div>' +
                            '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + pct.toFixed(0) + '%</span></div></div>';
                    }
                    const actions = '<div class="row-actions">' +
                        '<button class="btn btn-sm btn-icon btn-secondary" onclick="refreshAccountModels(\'' + a.id + '\')" title="' + t('providers.refreshModels') + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15"/></svg></button>' +
                        '<button class="btn btn-sm btn-secondary" onclick="editProviderAccount(\'' + a.id + '\')">' + t('common.edit') + '</button>' +
                        '<button class="btn btn-sm ' + (a.enabled ? 'btn-secondary' : 'btn-primary') + '" onclick="toggleAccount(\'' + a.id + '\',' + !a.enabled + ')">' + (a.enabled ? t('accounts.disable') : t('accounts.enable')) + '</button>' +
                        '<button class="btn btn-sm btn-secondary" onclick="testAccount(\'' + a.id + '\')" id="test-' + a.id + '" style="background:var(--accent);color:var(--on-accent);border-color:var(--accent)">' + t('accounts.test') + '</button>' +
                        '<button class="btn btn-sm btn-danger" onclick="deleteProviderAccount(\'' + a.id + '\')">' + t('accounts.delete') + '</button>' +
                        '</div>';
                    return '<tr>' +
                        '<td style="width:32px;text-align:center"><input type="checkbox" class="prov-row-cb" data-id="' + a.id + '" onclick="toggleProviderRow(\'' + a.id + '\',this.checked)"' + (providerSelection.has(a.id) ? ' checked' : '') + '></td>' +
                        '<td><div class="acct-email">' + escapeHtml(name) + '</div></td>' +
                        '<td><div class="row-meta">' + statusBadge + enabledBadge + planBadge + modelsBadge + limitBadge + '</div></td>' +
                        '<td class="cell-num">' + (a.requestCount || 0) + '</td>' +
                        '<td class="cell-num">' + formatNum(a.totalTokens || 0) + '</td>' +
                        '<td>' + quotaCell + '</td>' +
                        '<td>' + actions + '</td>' +
                        '</tr>';
                }).join('');
                const selectedInGroup = accts.filter(a => providerSelection.has(a.id)).length;
                const allChecked = selectedInGroup === accts.length && accts.length > 0;
                const groupIds = accts.map(a => a.id);
                // Card subtitle: account count, model count, aggregate token usage, and an
                // exhausted-key warning when any key in the card is spent.
                let usageStr = formatNum(usedSum) + ' ' + t('accounts.tokens');
                if (limitSum > 0) usageStr += ' / ' + formatNum(limitSum) + ' ' + t('providers.tokLimit');
                if (quotaLimitSum > 0) usageStr += ' · ' + t('accounts.mainQuota') + ' ' + quotaUsedSum.toFixed(0) + ' / ' + quotaLimitSum.toFixed(0);
                let exhaustedStr = exhausted > 0
                    ? ' · <span style="color:var(--danger,#dc2626)">' + t('providers.tokExhaustedN', exhausted) + '</span>'
                    : '';
                // Collapse the row table by default so a 100-key provider reads as
                // ONE compact card (count + models + aggregate usage in the header),
                // not 100 rows. Operator clicks the header to expand and manage keys.
                // Use class="hidden" (not DOM removal) so the select-all checkbox and
                // .prov-row-cb nodes survive for syncProviderGroupHeaders / selection.
                const expanded = providerCardExpanded.has(backend);
                const caret = expanded ? '▾' : '▸';
                return '<div class="card" style="margin:8px 0">' +
                    '<div class="card-header" style="padding:12px 16px;cursor:pointer" onclick=\'toggleProviderCard(' + JSON.stringify(backend) + ')\'>' +
                    '<span class="card-title" style="font-size:15px"><span style="display:inline-block;width:14px;color:#94a3b8">' + caret + '</span> ' + escapeHtml(label) +
                    ' <span style="font-weight:normal;color:#94a3b8;font-size:13px">(' + backend + ' · ' + accts.length + ' ' + t('providers.accounts') + ' · ' + totalModels + ' ' + t('providers.models') + ' · ' + usageStr + exhaustedStr + ')</span></span>' +
                    '</div>' +
                    '<div class="table-wrap' + (expanded ? '' : ' hidden') + '"><table class="data-table"><thead><tr>' +
                    '<th style="width:32px;text-align:center"><input type="checkbox" title="' + t('providers.selectAll') + '" onclick=\'toggleProviderGroup(' + JSON.stringify(groupIds) + ',this.checked)\'' + (allChecked ? ' checked' : '') + '></th>' +
                    '<th>' + t('providers.account') + '</th><th>' + t('providers.status') + '</th>' +
                    '<th class="cell-num">' + t('accounts.requests') + '</th><th class="cell-num">' + t('accounts.tokens') + '</th>' +
                    '<th>' + t('accounts.mainQuota') + '</th>' +
                    '<th style="text-align:right"> </th></tr></thead><tbody>' + rows + '</tbody></table></div>' +
                    '</div>';
            }).join('');
            container.innerHTML = html;
            updateProviderBatchBar();
        }
        // deleteProviderAccount deletes a non-Kiro provider account and re-renders
        // the Providers tab (deleteAccount re-renders the Accounts tab instead).
        async function deleteProviderAccount(id) {
            if (!await uiConfirm(t('accounts.confirmDelete'), { danger: true })) return;
            await fetch('/admin/api/accounts/' + id, { method: 'DELETE', headers: { 'X-Admin-Password': password } });
            providerSelection.delete(id);
            await loadAccounts();
            renderProviders();
            loadStats();
        }
        // ---- Providers: multi-select + batch delete ----
        // providerSelection holds the ids the operator ticked. It survives the 5s
        // auto-refresh re-render (renderProviders re-applies it and prunes dead ids),
        // so a long selection isn't lost mid-task.
        const providerSelection = new Set();
        // providerCardExpanded holds the backends whose key table is expanded. A
        // provider card is COLLAPSED by default so 100 keys read as one compact row;
        // clicking the header expands it. It survives the 5s auto-refresh re-render.
        const providerCardExpanded = new Set();
        function toggleProviderCard(backend) {
            if (providerCardExpanded.has(backend)) providerCardExpanded.delete(backend);
            else providerCardExpanded.add(backend);
            renderProviders();
        }
        function toggleProviderRow(id, checked) {
            if (checked) providerSelection.add(id); else providerSelection.delete(id);
            syncProviderGroupHeaders();
            updateProviderBatchBar();
        }
        function toggleProviderGroup(ids, checked) {
            (ids || []).forEach(id => { if (checked) providerSelection.add(id); else providerSelection.delete(id); });
            // Reflect on the visible row checkboxes without a full re-render.
            (ids || []).forEach(id => {
                const cb = document.querySelector('.prov-row-cb[data-id="' + id + '"]');
                if (cb) cb.checked = checked;
            });
            updateProviderBatchBar();
        }
        // syncProviderGroupHeaders re-evaluates each card's header checkbox so it
        // shows checked only when every row in that card is selected.
        function syncProviderGroupHeaders() {
            document.querySelectorAll('#providersList table').forEach(tbl => {
                const rowCbs = tbl.querySelectorAll('.prov-row-cb');
                const head = tbl.querySelector('thead input[type="checkbox"]');
                if (!head || rowCbs.length === 0) return;
                let all = true;
                rowCbs.forEach(cb => { if (!cb.checked) all = false; });
                head.checked = all;
            });
        }
        function clearProviderSelection() {
            providerSelection.clear();
            document.querySelectorAll('#providersList input[type="checkbox"]').forEach(cb => { cb.checked = false; });
            updateProviderBatchBar();
        }
        function updateProviderBatchBar() {
            const bar = document.getElementById('providerBatchBar');
            const count = document.getElementById('providerBatchCount');
            if (!bar) return;
            const n = providerSelection.size;
            bar.classList.toggle('hidden', n === 0);
            if (count) count.textContent = t('providers.nSelected', n);
        }
        // batchDeleteProviders deletes every selected account in one request, then
        // refreshes the tab. One confirm, one round-trip, one pool reload.
        async function batchDeleteProviders() {
            const ids = Array.from(providerSelection);
            if (ids.length === 0) return;
            if (!await uiConfirm(t('providers.confirmDeleteN', ids.length), { danger: true })) return;
            try {
                const res = await fetch('/admin/api/accounts/batch-delete', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ ids })
                });
                const d = await res.json();
                if (!res.ok || d.error) { alert(d.error || t('common.failed')); return; }
                // Hide the bar immediately (clears the set, unchecks boxes, and
                // re-applies the hidden class) so it can't linger if the subsequent
                // reload/render throws.
                clearProviderSelection();
                await loadAccounts();
                renderProviders();
                loadStats();
            } catch (e) {
                alert(t('common.failed'));
            }
        }
        // editProviderAccount opens a modal to edit a non-Kiro provider account:
        // name, weight, API key (rotate), base URL override, and pinned model
        // list. Custom (bring-your-own) accounts also show their dialect read-only.
        function editProviderAccount(id) {
            const a = (accountsData || []).find(x => x.id === id);
            if (!a) { alert(t('common.failed')); return; }
            const modal = document.getElementById('addModal');
            const title = document.getElementById('modalTitle');
            const body = document.getElementById('modalBody');
            const isCustom = (a.customDialect || '').trim() !== '';
            const backend = a.backend || '';
            const models = (a.customModels && a.customModels.length ? a.customModels : (a.modelList || []));
            title.textContent = t('providers.editTitle');
            const esc = s => escapeHtml(s == null ? '' : String(s));
            body.innerHTML =
                '<p style="font-size:13px;color:var(--muted);margin-bottom:14px">' + t('providers.editHint', esc(backend)) + '</p>' +
                '<div class="form-group"><label>' + t('providers.fName') + '</label><input type="text" id="edpName" value="' + esc(a.nickname || a.email || '') + '"></div>' +
                '<div class="form-group"><label>' + t('providers.fWeight') + '</label><input type="number" id="edpWeight" min="0" value="' + (a.weight || 0) + '"></div>' +
                '<div class="form-group"><label>' + t('providers.fApiKey') + ' <span style="font-weight:normal;color:var(--muted);font-size:12px">(' + t('providers.fApiKeyHint') + ')</span></label><input type="password" id="edpApiKey" placeholder="' + (a.hasApiKey ? '•••••• ' + t('providers.fKeptIfBlank') : 'sk-...') + '"></div>' +
                (backend && backend !== 'custom' ?
                    '<div class="form-group" style="border-top:1px solid var(--border);padding-top:14px;margin-top:4px">' +
                    '<label>' + t('providers.bulkAddTitle', esc(backend)) + ' <span style="font-weight:normal;color:var(--muted);font-size:12px">(' + t('providers.bulkAddHint') + ')</span></label>' +
                    '<textarea id="edpBulkKeys" rows="5" oninput="updateEdpBulkCount()" placeholder="sk-key-one&#10;sk-key-two&#10;sk-key-three" style="width:100%;font-family:monospace;font-size:12px"></textarea>' +
                    '<div id="edpBulkCount" style="font-size:12px;color:var(--muted);margin-top:4px;min-height:16px"></div>' +
                    '<button class="btn btn-secondary btn-sm" id="edpBulkBtn" onclick="submitEditBulkKeys(\'' + esc(backend) + '\')">' + t('providers.bulkAddBtn') + '</button>' +
                    '<div id="edpBulkStatus" style="font-size:13px;color:var(--muted);margin-top:6px;min-height:18px"></div>' +
                    '</div>'
                    : '') +
                (isCustom || a.baseURLOverride ?
                    '<div class="form-group"><label>' + t('providers.fBaseURL') + (isCustom ? ' <span style="font-weight:normal;color:var(--muted);font-size:12px">(' + esc(a.customDialect) + ')</span>' : '') + '</label><input type="text" id="edpBaseURL" value="' + esc(a.baseURLOverride || '') + '" placeholder="https://api.example.com/v1"></div>'
                    : '<input type="hidden" id="edpBaseURL" value="">') +
                '<div class="form-group"><label>' + t('providers.fModels') + ' <span style="font-weight:normal;color:var(--muted);font-size:12px">(' + t('providers.fModelsHint') + ')</span></label><input type="text" id="edpModels" value="' + esc(models.join(', ')) + '" placeholder="gpt-4o, llama-3.3-70b"></div>' +
                '<div class="form-group"><label>' + t('providers.fTokenLimit') + ' <span style="font-weight:normal;color:var(--muted);font-size:12px">(' + t('providers.fTokenLimitHint') + (a.tokenLimit > 0 ? '; ' + t('providers.fTokenUsed', formatNum(a.totalTokens || 0)) : '') + ')</span></label><input type="number" id="edpTokenLimit" min="0" step="1000" value="' + (a.tokenLimit || 0) + '"></div>' +
                '<div id="edpStatus" style="font-size:13px;color:var(--muted);margin:8px 0;min-height:18px"></div>' +
                '<div class="modal-footer"><button class="btn btn-secondary" onclick="closeModal()">' + t('common.cancel') + '</button><button class="btn btn-primary" id="edpSaveBtn" onclick="submitEditProviderAccount(\'' + id + '\')">' + t('common.save') + '</button></div>';
            modal.classList.add('active');
        }
        async function submitEditProviderAccount(id) {
            const payload = {};
            const nm = document.getElementById('edpName').value.trim();
            payload.nickname = nm;
            payload.weight = parseInt(document.getElementById('edpWeight').value, 10) || 0;
            const key = document.getElementById('edpApiKey').value.trim();
            if (key) payload.apiKey = key; // blank = keep existing
            const baseEl = document.getElementById('edpBaseURL');
            if (baseEl) payload.baseURL = baseEl.value.trim();
            const modelsRaw = (document.getElementById('edpModels').value || '').trim();
            payload.models = modelsRaw ? modelsRaw.split(',').map(s => s.trim()).filter(Boolean) : [];
            const tlEl = document.getElementById('edpTokenLimit');
            if (tlEl) { const tl = parseInt(tlEl.value, 10); payload.tokenLimit = (!isNaN(tl) && tl > 0) ? tl : 0; }
            const btn = document.getElementById('edpSaveBtn');
            const statusEl = document.getElementById('edpStatus');
            if (btn) { btn.disabled = true; btn.textContent = t('import.running'); }
            try {
                const res = await fetch('/admin/api/accounts/' + id, {
                    method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify(payload)
                });
                const d = await res.json();
                if (!res.ok || d.error) {
                    if (btn) { btn.disabled = false; btn.textContent = t('common.save'); }
                    if (statusEl) statusEl.textContent = d.error || t('common.failed');
                    return;
                }
                closeModal();
                await loadAccounts();
                if (activeDashboardTab() === 'providers') renderProviders();
                loadStats();
            } catch (e) {
                if (btn) { btn.disabled = false; btn.textContent = t('common.save'); }
                if (statusEl) statusEl.textContent = t('common.failed');
            }
        }
        function updateEdpBulkCount() {
            const ta = document.getElementById('edpBulkKeys');
            const out = document.getElementById('edpBulkCount');
            if (!ta || !out) return;
            const seen = {};
            let n = 0;
            ta.value.split(/\r?\n/).forEach(line => {
                const k = line.trim();
                if (k && !seen[k]) { seen[k] = true; n++; }
            });
            out.textContent = n === 0 ? '' : (n + ' unique key' + (n === 1 ? '' : 's') + ' will be added');
        }
        async function submitEditBulkKeys(backend) {
            const ta = document.getElementById('edpBulkKeys');
            const statusEl = document.getElementById('edpBulkStatus');
            const btn = document.getElementById('edpBulkBtn');
            if (!ta) return;
            const seen = {};
            const apiKeys = [];
            ta.value.split(/\r?\n/).forEach(line => {
                const k = line.trim();
                if (k && !seen[k]) { seen[k] = true; apiKeys.push(k); }
            });
            if (apiKeys.length === 0) { if (statusEl) statusEl.textContent = 'Paste at least one API key (one per line)'; return; }
            if (btn) { btn.disabled = true; btn.textContent = t('import.running'); }
            if (statusEl) statusEl.textContent = 'Adding ' + apiKeys.length + ' accounts…';
            try {
                const res = await fetch('/admin/api/providers/account/bulk', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify({ backend: backend, apiKeys: apiKeys })
                });
                const d = await res.json();
                if (d.success) {
                    closeModal();
                    await loadAccounts();
                    if (activeDashboardTab() === 'providers') renderProviders();
                    loadStats();
                } else {
                    if (btn) { btn.disabled = false; btn.textContent = t('providers.bulkAddBtn'); }
                    if (statusEl) statusEl.textContent = d.error || t('common.failed');
                }
            } catch (e) {
                if (btn) { btn.disabled = false; btn.textContent = t('providers.bulkAddBtn'); }
                if (statusEl) statusEl.textContent = t('common.failed');
            }
        }
        let codexSessionId = '';
        async function startCodexLogin() {
            const res = await fetch('/admin/api/auth/codex/start', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: '{}' });
            const d = await res.json();
            if (!res.ok || d.error) { alert(d.error || 'Failed to start Codex login'); return; }
            codexSessionId = d.sessionId;
            document.getElementById('codexStep1').classList.add('hidden');
            document.getElementById('codexStep2').classList.remove('hidden');
            document.getElementById('codexAuthUrl').textContent = d.authUrl;
            window.open(d.authUrl, '_blank');
            pollCodexLogin();
        }
        async function pollCodexLogin() {
            if (!codexSessionId) return;
            try {
                const res = await fetch('/admin/api/auth/codex/poll', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ sessionId: codexSessionId }) });
                const d = await res.json();
                if (d.status === 'completed') { cancelCodexLogin(); closeModal(); loadAccounts(); return; }
                if (d.status === 'error') { document.getElementById('codexStatus').textContent = 'Error: ' + (d.error || 'login failed'); return; }
            } catch (e) { /* keep polling */ }
            codexPollTimer = setTimeout(pollCodexLogin, 2000);
        }
        function cancelCodexLogin() {
            if (codexPollTimer) { clearTimeout(codexPollTimer); codexPollTimer = null; }
            codexSessionId = '';
        }
        async function importCodexToken() {
            const accessToken = document.getElementById('codexToken').value.trim();
            const name = document.getElementById('codexName').value.trim();
            if (!accessToken) { alert('Paste an access/id token, or use Login with OpenAI'); return; }
            const res = await fetch('/admin/api/auth/codex/token', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ accessToken, name }) });
            const d = await res.json();
            if (d.success) { closeModal(); loadAccounts(); } else { alert(d.error || 'Failed to add Codex account'); }
        }
        let qoderPollTimer = null;
        let qoderSessionId = '';
        async function startQoderLogin() {
            const res = await fetch('/admin/api/auth/qoder/start', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: '{}' });
            const d = await res.json();
            if (!res.ok || d.error) { alert(d.error || 'Failed to start Qoder login'); return; }
            qoderSessionId = d.sessionId;
            document.getElementById('qoderStep1').classList.add('hidden');
            document.getElementById('qoderStep2').classList.remove('hidden');
            document.getElementById('qoderAuthUrl').textContent = d.authUrl;
            window.open(d.authUrl, '_blank');
            pollQoderLogin();
        }
        async function pollQoderLogin() {
            if (!qoderSessionId) return;
            try {
                const res = await fetch('/admin/api/auth/qoder/poll', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ sessionId: qoderSessionId }) });
                const d = await res.json();
                if (d.status === 'completed') { cancelQoderLogin(); closeModal(); loadAccounts(); return; }
                if (d.status === 'error') { document.getElementById('qoderStatus').textContent = 'Error: ' + (d.error || 'login failed'); return; }
            } catch (e) { /* keep polling */ }
            qoderPollTimer = setTimeout(pollQoderLogin, 2000);
        }
        function cancelQoderLogin() {
            if (qoderPollTimer) { clearTimeout(qoderPollTimer); qoderPollTimer = null; }
            qoderSessionId = '';
        }
        let qwenPollTimer = null;
        let qwenSessionId = '';
        async function startQwenLogin() {
            const res = await fetch('/admin/api/auth/qwen/start', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: '{}' });
            const d = await res.json();
            if (!res.ok || d.error) { alert(d.error || 'Failed to start Qwen login'); return; }
            qwenSessionId = d.sessionId;
            document.getElementById('qwenStep1').classList.add('hidden');
            document.getElementById('qwenStep2').classList.remove('hidden');
            document.getElementById('qwenUserCode').textContent = d.userCode || '';
            const url = d.verificationUriComplete || d.verificationUri || '';
            document.getElementById('qwenAuthUrl').textContent = url;
            if (url) window.open(url, '_blank');
            pollQwenLogin();
        }
        async function pollQwenLogin() {
            if (!qwenSessionId) return;
            try {
                const res = await fetch('/admin/api/auth/qwen/poll', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ sessionId: qwenSessionId }) });
                const d = await res.json();
                if (d.status === 'completed') { cancelQwenLogin(); closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (d.status === 'error') { document.getElementById('qwenStatus').textContent = 'Error: ' + (d.error || 'login failed'); return; }
            } catch (e) { /* keep polling */ }
            qwenPollTimer = setTimeout(pollQwenLogin, 2500);
        }
        function cancelQwenLogin() {
            if (qwenPollTimer) { clearTimeout(qwenPollTimer); qwenPollTimer = null; }
            qwenSessionId = '';
        }

        // ===== Generic OAuth / browser-login / token-import connect flows =====
        // Each catalog provider with authType "oauth" maps to one of these flow
        // shapes. The dispatcher (openOAuthProvider) renders the right modal body
        // and the *Generic* helpers drive start/poll/complete/import against the
        // provider's /admin/api/auth/<id>/* endpoints. This replaces hand-writing a
        // bespoke modal + JS per provider.
        //
        // kind:
        //   'device'   — start returns {sessionId, userCode?, verificationUri}; poll until completed
        //   'loopback' — start returns {sessionId, authUrl}; browser callback completes server-side; poll until completed
        //   'paste'    — start returns {sessionId, authUrl}; user pastes the redirect code; complete exchanges it
        //   'gitlab'   — like 'paste' but start needs {baseUrl, clientId, clientSecret}
        //   'token'    — no start; import a pasted token (+ optional fields)
        //   'cookie'   — no start; import a session cookie
        //   'vertex'   — no start; import a service-account JSON
        const OAUTH_FLOWS = {
            codebuddy:      { kind: 'device',   kindLabel: 'browser login' },
            'codebuddy-ai': { kind: 'device',   kindLabel: 'browser login' },
            'kimi-coding':  { kind: 'device',   kindLabel: 'device code' },
            kilocode:       { kind: 'device',   kindLabel: 'browser login' },
            github:         { kind: 'device',   kindLabel: 'device code' },
            qwen:           { kind: 'device',   kindLabel: 'device code' },
            xai:            { kind: 'loopback', kindLabel: 'browser login' },
            iflow:          { kind: 'loopback', kindLabel: 'browser login' },
            'gemini-cli':   { kind: 'loopback', kindLabel: 'Google login' },
            antigravity:    { kind: 'loopback', kindLabel: 'Google login' },
            'claude-code':  { kind: 'paste',    kindLabel: 'paste code' },
            cline:          { kind: 'paste',    kindLabel: 'paste code' },
            gitlab:         { kind: 'gitlab',   kindLabel: 'OAuth app' },
            cursor:         { kind: 'token',    kindLabel: 'import token',
                fields: [
                    { id: 'accessToken', label: 'Access Token', type: 'password', placeholder: 'cursorAuth/accessToken from the Cursor IDE', required: true },
                    { id: 'machineId',   label: 'Machine ID',  type: 'text',     placeholder: 'storage.serviceMachineId from the Cursor IDE', required: true },
                    { id: 'name',        label: 'Name (optional)', type: 'text',  placeholder: 'Cursor IDE' }
                ],
                importPath: '/auth/cursor/import',
                help: 'Import from the Cursor IDE state DB: cursorAuth/accessToken + storage.serviceMachineId.' },
            vertex:         { kind: 'vertex',   kindLabel: 'service account',
                importPath: '/auth/vertex/import',
                help: 'Paste a Google Cloud service-account JSON key with Vertex AI access.' },
            'grok-web':       { kind: 'cookie', kindLabel: 'session cookie',
                importPath: '/auth/cookie/import',
                help: 'Paste your grok.com session cookie (the sso cookie value).' },
            'perplexity-web': { kind: 'cookie', kindLabel: 'session cookie',
                importPath: '/auth/cookie/import',
                help: 'Paste your perplexity.ai session cookie value.' }
        };
        let oauthFlowTimer = null;
        let oauthFlowSession = '';
        let oauthFlowId = '';
        function cancelOAuthFlow() {
            if (oauthFlowTimer) { clearTimeout(oauthFlowTimer); oauthFlowTimer = null; }
            oauthFlowSession = '';
            oauthFlowId = '';
        }
        function oauthHeaders() {
            return { 'Content-Type': 'application/json', 'X-Admin-Password': password };
        }

        // Render the connect modal for a provider id.
        async function openOAuthProvider(id) {
            const flow = OAUTH_FLOWS[id];
            if (!flow) { alert('No connect flow for ' + id); return; }
            cancelOAuthFlow();
            oauthFlowId = id;
            const modal = document.getElementById('addModal');
            const title = document.getElementById('modalTitle');
            const body = document.getElementById('modalBody');
            // Friendly display name from the loaded catalog if available.
            let name = id;
            try {
                const r = await fetch('/admin/api/providers/catalog', { headers: { 'X-Admin-Password': password } });
                const d = await r.json();
                const p = (d.providers || []).find(x => x.id === id);
                if (p && p.name) name = p.name;
            } catch (e) {}
            title.textContent = 'Connect ' + name;
            const back = '<button class="btn btn-secondary" onclick="showAddProviderModal()">' + t('common.back') + '</button>';
            const help = flow.help ? '<p style="font-size:12px;color:var(--muted);margin-bottom:14px">' + flow.help + '</p>' : '';

            if (flow.kind === 'device' || flow.kind === 'loopback') {
                body.innerHTML = help +
                    '<div id="oauthStep1"><div class="modal-footer">' + back +
                    '<button class="btn btn-primary" onclick="startOAuthFlow(\'' + id + '\')">Start login</button></div></div>' +
                    '<div id="oauthStep2" class="hidden">' +
                    '<div id="oauthCodeBox" class="message hidden" style="background:#eff6ff;color:#1d4ed8;text-align:center"><p style="font-size:12px;margin-bottom:6px">Enter this code on the verification page:</p><p style="font-size:22px;font-weight:700;letter-spacing:2px" id="oauthUserCode"></p></div>' +
                    '<div class="form-group" style="margin-top:12px"><label>Verification URL</label><div class="endpoint" style="margin-bottom:0"><span id="oauthAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'oauthAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'oauthAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<p id="oauthStatus" style="color:#64748b;margin:16px 0;font-size:13px;text-align:center">Waiting for you to authorize in the browser…</p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelOAuthFlow();showAddProviderModal()">' + t('common.cancel') + '</button></div>' +
                    '</div>';
            } else if (flow.kind === 'paste') {
                body.innerHTML = help +
                    '<div id="oauthStep1"><div class="modal-footer">' + back +
                    '<button class="btn btn-primary" onclick="startOAuthFlow(\'' + id + '\')">Start login</button></div></div>' +
                    '<div id="oauthStep2" class="hidden">' +
                    '<div class="form-group"><label>Authorization URL</label><div class="endpoint" style="margin-bottom:0"><span id="oauthAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'oauthAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'oauthAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<div class="form-group" style="margin-top:12px"><label>Paste the code from the redirect</label><input type="text" id="oauthCode" placeholder="Paste the authorization code here"></div>' +
                    '<p id="oauthStatus" style="color:#64748b;margin:8px 0;font-size:13px;min-height:16px"></p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelOAuthFlow();showAddProviderModal()">' + t('common.cancel') + '</button>' +
                    '<button class="btn btn-primary" onclick="completeOAuthFlow(\'' + id + '\')">Complete</button></div>' +
                    '</div>';
            } else if (flow.kind === 'gitlab') {
                body.innerHTML = help +
                    '<div id="oauthStep1">' +
                    '<div class="form-group"><label>GitLab instance URL <span style="font-weight:normal;color:#64748b;font-size:12px">(blank = gitlab.com)</span></label><input type="text" id="glBaseUrl" placeholder="https://gitlab.com"></div>' +
                    '<div class="form-group"><label>OAuth App Client ID</label><input type="text" id="glClientId" placeholder="Application ID from your GitLab OAuth app"></div>' +
                    '<div class="form-group"><label>OAuth App Client Secret <span style="font-weight:normal;color:#64748b;font-size:12px">(optional for PKCE apps)</span></label><input type="password" id="glClientSecret" placeholder="Secret"></div>' +
                    '<div class="modal-footer">' + back + '<button class="btn btn-primary" onclick="startOAuthFlow(\'gitlab\')">Start login</button></div></div>' +
                    '<div id="oauthStep2" class="hidden">' +
                    '<div class="form-group"><label>Authorization URL</label><div class="endpoint" style="margin-bottom:0"><span id="oauthAuthUrl" style="font-size:11px;word-break:break-all"></span></div>' +
                    '<div style="display:flex;gap:8px;margin-top:8px"><button class="btn btn-sm btn-secondary" style="flex:1" onclick="window.open(document.getElementById(\'oauthAuthUrl\').textContent,\'_blank\')">Open</button><button class="btn btn-sm btn-secondary" style="flex:1" onclick="navigator.clipboard.writeText(document.getElementById(\'oauthAuthUrl\').textContent);alert(\'' + t('common.copied') + '\')">' + t('common.copy') + '</button></div></div>' +
                    '<div class="form-group" style="margin-top:12px"><label>Paste the code from the redirect</label><input type="text" id="oauthCode" placeholder="Paste the authorization code here"></div>' +
                    '<p id="oauthStatus" style="color:#64748b;margin:8px 0;font-size:13px;min-height:16px"></p>' +
                    '<div class="modal-footer"><button class="btn btn-secondary" onclick="cancelOAuthFlow();showAddProviderModal()">' + t('common.cancel') + '</button>' +
                    '<button class="btn btn-primary" onclick="completeOAuthFlow(\'gitlab\')">Complete</button></div>' +
                    '</div>';
            } else if (flow.kind === 'token') {
                const rows = (flow.fields || []).map(f =>
                    '<div class="form-group"><label>' + f.label + '</label><input type="' + (f.type || 'text') + '" id="oauthField_' + f.id + '" placeholder="' + (f.placeholder || '') + '"></div>').join('');
                body.innerHTML = help + rows +
                    '<p id="oauthStatus" style="color:#64748b;margin:8px 0;font-size:13px;min-height:16px"></p>' +
                    '<div class="modal-footer">' + back +
                    '<button class="btn btn-primary" onclick="importOAuthToken(\'' + id + '\')">' + t('common.add') + '</button></div>';
            } else if (flow.kind === 'vertex') {
                body.innerHTML = help +
                    '<div class="form-group"><label>Service Account JSON</label><textarea id="oauthVertexJson" rows="8" style="width:100%;font-family:ui-monospace,Menlo,monospace;font-size:12px;resize:vertical" placeholder=\'{ "type": "service_account", ... }\'></textarea></div>' +
                    '<div class="form-group"><label>Region <span style="font-weight:normal;color:#64748b;font-size:12px">(default us-central1)</span></label><input type="text" id="oauthVertexRegion" placeholder="us-central1"></div>' +
                    '<div class="form-group"><label>Name (optional)</label><input type="text" id="oauthVertexName" placeholder="Vertex AI"></div>' +
                    '<p id="oauthStatus" style="color:#64748b;margin:8px 0;font-size:13px;min-height:16px"></p>' +
                    '<div class="modal-footer">' + back +
                    '<button class="btn btn-primary" onclick="importVertexAccount()">' + t('common.add') + '</button></div>';
            } else if (flow.kind === 'cookie') {
                body.innerHTML = help +
                    '<div class="form-group"><label>Session Cookie</label><textarea id="oauthCookie" rows="4" style="width:100%;font-family:ui-monospace,Menlo,monospace;font-size:12px;resize:vertical" placeholder="Paste the cookie value"></textarea></div>' +
                    '<div class="form-group"><label>Name (optional)</label><input type="text" id="oauthCookieName" placeholder="' + name + '"></div>' +
                    '<p id="oauthStatus" style="color:#64748b;margin:8px 0;font-size:13px;min-height:16px"></p>' +
                    '<div class="modal-footer">' + back +
                    '<button class="btn btn-primary" onclick="importCookieAccount(\'' + id + '\')">' + t('common.add') + '</button></div>';
            }
            modal.classList.add('active');
        }
        // device + loopback + paste + gitlab share this start.
        async function startOAuthFlow(id) {
            const flow = OAUTH_FLOWS[id];
            let body = '{}';
            if (flow.kind === 'gitlab') {
                const clientId = document.getElementById('glClientId').value.trim();
                if (!clientId) { alert('Client ID is required (register an OAuth app on your GitLab instance)'); return; }
                body = JSON.stringify({
                    baseUrl: document.getElementById('glBaseUrl').value.trim(),
                    clientId: clientId,
                    clientSecret: document.getElementById('glClientSecret').value.trim()
                });
            }
            let res, d;
            try {
                res = await fetch('/admin/api/auth/' + id + '/start', { method: 'POST', headers: oauthHeaders(), body });
                d = await res.json();
            } catch (e) { alert('Failed to start login'); return; }
            if (!res.ok || d.error) { alert(d.error || 'Failed to start login'); return; }
            oauthFlowSession = d.sessionId || '';
            document.getElementById('oauthStep1').classList.add('hidden');
            document.getElementById('oauthStep2').classList.remove('hidden');
            const url = d.verificationUriComplete || d.verificationUri || d.authUrl || '';
            document.getElementById('oauthAuthUrl').textContent = url;
            const codeBox = document.getElementById('oauthCodeBox');
            if (d.userCode && codeBox) {
                codeBox.classList.remove('hidden');
                document.getElementById('oauthUserCode').textContent = d.userCode;
            }
            if (url) window.open(url, '_blank');
            // device + loopback poll; paste/gitlab wait for the user to submit a code.
            if (flow.kind === 'device' || flow.kind === 'loopback') pollOAuthFlow(id);
        }
        async function pollOAuthFlow(id) {
            if (!oauthFlowSession || oauthFlowId !== id) return;
            try {
                const res = await fetch('/admin/api/auth/' + id + '/poll', { method: 'POST', headers: oauthHeaders(), body: JSON.stringify({ sessionId: oauthFlowSession }) });
                const d = await res.json();
                if (d.status === 'completed') { cancelOAuthFlow(); closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (d.status === 'error') { const s = document.getElementById('oauthStatus'); if (s) s.textContent = 'Error: ' + (d.error || 'login failed'); return; }
            } catch (e) { /* keep polling */ }
            oauthFlowTimer = setTimeout(() => pollOAuthFlow(id), 2500);
        }
        async function completeOAuthFlow(id) {
            const code = document.getElementById('oauthCode').value.trim();
            if (!code) { alert('Paste the authorization code first'); return; }
            const statusEl = document.getElementById('oauthStatus');
            if (statusEl) statusEl.textContent = t('import.running');
            try {
                const res = await fetch('/admin/api/auth/' + id + '/complete', { method: 'POST', headers: oauthHeaders(), body: JSON.stringify({ sessionId: oauthFlowSession, code }) });
                const d = await res.json();
                if (d.status === 'completed' || d.success) { cancelOAuthFlow(); closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (statusEl) statusEl.textContent = 'Error: ' + (d.error || 'login failed');
            } catch (e) { if (statusEl) statusEl.textContent = 'Error: request failed'; }
        }
        async function importOAuthToken(id) {
            const flow = OAUTH_FLOWS[id];
            const payload = {};
            for (const f of (flow.fields || [])) {
                const v = document.getElementById('oauthField_' + f.id).value.trim();
                if (f.required && !v) { alert(f.label + ' is required'); return; }
                if (v) payload[f.id] = v;
            }
            const statusEl = document.getElementById('oauthStatus');
            if (statusEl) statusEl.textContent = t('import.running');
            try {
                const res = await fetch('/admin/api' + flow.importPath, { method: 'POST', headers: oauthHeaders(), body: JSON.stringify(payload) });
                const d = await res.json();
                if (d.success || d.id) { closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (statusEl) statusEl.textContent = 'Error: ' + (d.error || 'import failed');
            } catch (e) { if (statusEl) statusEl.textContent = 'Error: request failed'; }
        }
        async function importVertexAccount() {
            const serviceAccountJson = document.getElementById('oauthVertexJson').value.trim();
            if (!serviceAccountJson) { alert('Paste the service-account JSON'); return; }
            const payload = {
                serviceAccountJson,
                region: document.getElementById('oauthVertexRegion').value.trim(),
                name: document.getElementById('oauthVertexName').value.trim()
            };
            const statusEl = document.getElementById('oauthStatus');
            if (statusEl) statusEl.textContent = t('import.running');
            try {
                const res = await fetch('/admin/api/auth/vertex/import', { method: 'POST', headers: oauthHeaders(), body: JSON.stringify(payload) });
                const d = await res.json();
                if (d.success || d.id) { closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (statusEl) statusEl.textContent = 'Error: ' + (d.error || 'import failed');
            } catch (e) { if (statusEl) statusEl.textContent = 'Error: request failed'; }
        }
        async function importCookieAccount(id) {
            const cookie = document.getElementById('oauthCookie').value.trim();
            if (!cookie) { alert('Paste the session cookie'); return; }
            const payload = { backend: id, cookie, name: document.getElementById('oauthCookieName').value.trim() };
            const statusEl = document.getElementById('oauthStatus');
            if (statusEl) statusEl.textContent = t('import.running');
            try {
                const res = await fetch('/admin/api/auth/cookie/import', { method: 'POST', headers: oauthHeaders(), body: JSON.stringify(payload) });
                const d = await res.json();
                if (d.success || d.id) { closeModal(); loadAccounts(); if (activeDashboardTab() === 'providers') loadProviders(); return; }
                if (statusEl) statusEl.textContent = 'Error: ' + (d.error || 'import failed');
            } catch (e) { if (statusEl) statusEl.textContent = 'Error: request failed'; }
        }
        function loadLocalFile(input, targetId) {
            const file = input.files[0];
            if (!file) return;
            const reader = new FileReader();
            reader.onload = e => { document.getElementById(targetId).value = e.target.result; };
            reader.readAsText(file);
        }
        function updateLocalFields() {
            const provider = document.getElementById('localProvider').value;
            const clientGroup = document.getElementById('localClientGroup');
            clientGroup.style.display = (provider === 'Google' || provider === 'Github') ? 'none' : 'block';
        }
        async function importLocalKiro() {
            const provider = document.getElementById('localProvider').value;
            const tokenJson = document.getElementById('localTokenJson').value.trim();
            const clientJson = document.getElementById('localClientJson').value.trim();
            const isSocial = provider === 'Google' || provider === 'Github';
            if (!tokenJson) { alert(t('local.tokenMissing')); return; }
            let tokenData, clientData;
            try { tokenData = JSON.parse(tokenJson); } catch { alert(t('local.tokenInvalid')); return; }
            if (!tokenData.refreshToken) { alert(t('local.refreshTokenMissing')); return; }
            if (!isSocial) {
                if (!clientJson) { alert(t('local.clientMissing')); return; }
                try { clientData = JSON.parse(clientJson); } catch { alert(t('local.clientInvalid')); return; }
                if (!clientData.clientId || !clientData.clientSecret) { alert(t('local.clientSecretMissing')); return; }
            }
            // 根据是否有 clientData 判断认证方式
            const authMethod = clientData ? 'idc' : 'social';
            const payload = { refreshToken: tokenData.refreshToken, accessToken: tokenData.accessToken || '', clientId: clientData?.clientId || '', clientSecret: clientData?.clientSecret || '', authMethod: authMethod, provider: provider };
            const res = await fetch('/admin/api/auth/credentials', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify(payload) });
            const d = await res.json();
            if (d.success) { closeModal(); loadAccounts(); loadStats(); alert(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id)); autoRefreshNewAccount(d.account?.id); }
            else alert(t('common.failed') + ': ' + d.error);
        }
        async function importCredentials() {
            try {
                const json = JSON.parse(document.getElementById('credJson').value.trim());
                // 兼容 Kiro Account Manager 导出格式 {version, accounts: [...]}
                let items;
                if (json.accounts && Array.isArray(json.accounts)) {
                    // AccountExportData 格式，从 accounts[].credentials 提取
                    items = json.accounts.map(a => {
                        const c = a.credentials || {};
                        return {
                            refreshToken: c.refreshToken || a.refreshToken,
                            clientId: c.clientId || a.clientId,
                            clientSecret: c.clientSecret || a.clientSecret,
                            region: c.region || a.region,
                            // 不传 accessToken，强制后端用 refreshToken 刷新获取新 token
                            authMethod: c.authMethod || a.authMethod,
                            provider: c.provider || a.provider || a.idp
                        };
                    });
                } else {
                    items = Array.isArray(json) ? json : [json];
                }
                let success = 0, failed = 0, errors = [], newIds = [];
                for (const item of items) {
                    if (!item.refreshToken) { failed++; errors.push('missing refreshToken'); continue; }
                    // 映射 authMethod: IdC/idc -> idc, social -> social
                    let authMethod = item.authMethod || '';
                    if (item.clientId && item.clientSecret) {
                        authMethod = 'idc';
                    } else if (!authMethod || authMethod === 'social') {
                        authMethod = 'social';
                    } else {
                        authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
                    }
                    // 映射 provider
                    let provider = item.provider || '';
                    if (!provider && authMethod === 'social') provider = 'Google';
                    if (!provider && authMethod === 'idc') provider = 'BuilderId';
                    const payload = { refreshToken: item.refreshToken, accessToken: item.accessToken || '', clientId: item.clientId || '', clientSecret: item.clientSecret || '', authMethod: authMethod, provider: provider, region: item.region || 'us-east-1' };
                    try {
                        const res = await fetch('/admin/api/auth/credentials', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify(payload) });
                        const d = await res.json();
                        if (d.success) { success++; if (d.account?.id) newIds.push(d.account.id); } else { failed++; errors.push(d.error || 'unknown'); }
                    } catch { failed++; errors.push('request failed'); }
                }
                closeModal(); loadAccounts(); loadStats();
                let msg = t('sso.importSuccess', success);
                if (failed > 0) msg += t('sso.importPartial', failed);
                alert(msg);
                newIds.forEach(id => autoRefreshNewAccount(id));
            } catch (e) { alert(t('credentials.jsonError')); }
        }
        async function importFromCookie() {
            const refreshToken = document.getElementById('cookieRefreshToken').value.trim();
            if (!refreshToken) { alert(t('cookie.refreshTokenMissing')); return; }
            const provider = document.getElementById('cookieProvider').value;
            const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
            const res = await fetch('/admin/api/auth/credentials', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify(payload) });
            const d = await res.json();
            if (d.success) { closeModal(); loadAccounts(); loadStats(); alert(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id)); autoRefreshNewAccount(d.account?.id); }
            else alert(t('common.failed') + ': ' + d.error);
        }
        async function importSsoToken() {
            const res = await fetch('/admin/api/auth/sso-token', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ bearerToken: document.getElementById('ssoToken').value, region: document.getElementById('ssoRegion').value }) });
            const d = await res.json();
            if (d.success) {
                closeModal(); loadAccounts(); loadStats();
                const count = d.accounts?.length || 0;
                const errCount = d.errors?.length || 0;
                let msg = t('sso.importSuccess', count);
                if (errCount > 0) msg += t('sso.importPartial', errCount);
                alert(msg);
                if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
            } else alert(t('common.failed') + ': ' + d.error);
        }
        let builderIdSession = '';
        let builderIdPollTimer = null;
        async function startBuilderIdLogin() {
            const region = document.getElementById('builderIdRegion').value || 'us-east-1';
            const res = await fetch('/admin/api/auth/builderid/start', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ region }) });
            const d = await res.json();
            if (d.sessionId) {
                builderIdSession = d.sessionId;
                document.getElementById('builderIdUserCode').textContent = d.userCode;
                document.getElementById('builderIdVerifyUrl').textContent = d.verificationUri;
                document.getElementById('builderIdStep1').classList.add('hidden');
                document.getElementById('builderIdStep2').classList.remove('hidden');
                pollBuilderIdAuth(d.interval || 5);
            } else alert(t('common.failed') + ': ' + d.error);
        }
        function pollBuilderIdAuth(interval) {
            builderIdPollTimer = setTimeout(async () => {
                const res = await fetch('/admin/api/auth/builderid/poll', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ sessionId: builderIdSession }) });
                const d = await res.json();
                if (d.completed) {
                    closeModal(); loadAccounts(); loadStats();
                    alert(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
                    autoRefreshNewAccount(d.account?.id);
                } else if (d.success && !d.completed) {
                    document.getElementById('builderIdStatus').textContent = t('builderid.waiting');
                    pollBuilderIdAuth(d.interval || interval);
                } else {
                    alert(t('common.failed') + ': ' + d.error);
                    cancelBuilderIdLogin();
                }
            }, interval * 1000);
        }
        function cancelBuilderIdLogin() {
            if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
            builderIdSession = '';
            showModal('add');
        }
        let iamSession = '';
        async function startIamSso() {
            if (iamSession) {
                const res = await fetch('/admin/api/auth/iam-sso/complete', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ sessionId: iamSession, callbackUrl: document.getElementById('iamCallback').value }) });
                const d = await res.json();
                if (d.success) { closeModal(); loadAccounts(); loadStats(); alert(t('builderid.success') + ': ' + (d.account?.email || d.account?.id)); autoRefreshNewAccount(d.account?.id); }
                else alert(t('common.failed') + ': ' + d.error);
            } else {
                const res = await fetch('/admin/api/auth/iam-sso/start', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password }, body: JSON.stringify({ startUrl: document.getElementById('iamStartUrl').value, region: document.getElementById('iamRegion').value }) });
                const d = await res.json();
                if (d.authorizeUrl) {
                    iamSession = d.sessionId;
                    document.getElementById('iamAuthUrl').textContent = d.authorizeUrl;
                    document.getElementById('iamStep2').classList.remove('hidden');
                    document.getElementById('iamBtn').textContent = t('iam.complete');
                } else alert(t('common.failed') + ': ' + d.error);
            }
        }
        // Realtime status push via WebSocket (preferred), with polling
        // fallback. The WS handler authenticates via Sec-WebSocket-Protocol
        // because browsers cannot set custom headers on WS upgrades — we
        // pass the admin password as a subprotocol token and the server
        // echoes it back on accept. If the WS errors out (proxy in front
        // strips upgrades, server lacks the route, etc.), we fall back to
        // 3-second polling so the dashboard still updates.
        let dashboardWS = null;
        let dashboardWSReconnect = null;
        let dashboardPollFallback = null;
        function applyStatusSnapshot(d) {
            try { pingOvLive(); } catch (e) { /* defensive */ }
            try { renderStatusFromObject(d); } catch (e) { /* defensive */ }
            // Merge per-account live counters into accountsData so the account
            // cards (credits / tokens / requests / quota) update in realtime
            // without the operator hitting refresh. The push carries only the
            // live-changing fields; we patch them onto the existing rows and
            // re-render. Structural fields (email, weight, proxy, etc.) come
            // from the full loadAccounts() fetch and are left untouched.
            try { mergeAccountStats(d.accountStats); } catch (e) { /* defensive */ }
        }
        function mergeAccountStats(stats) {
            if (!Array.isArray(stats) || !Array.isArray(accountsData) || accountsData.length === 0) return;
            const byId = {};
            for (const s of stats) byId[s.id] = s;
            let changed = false;
            for (const a of accountsData) {
                const s = byId[a.id];
                if (!s) continue;
                // Patch only the live counters/quota the cards render.
                const fields = ['enabled','banStatus','expiresAt','hasToken',
                    'usageCurrent','usageLimit','usagePercent',
                    'trialUsageCurrent','trialUsageLimit','trialUsagePercent',
                    'requestCount','errorCount','totalTokens','totalCredits',
                    'lastUsed','lastRefresh','inflight','concurrencyLimit',
                    'cooldownSecs','overQuota'];
                for (const f of fields) {
                    if (s[f] !== undefined && a[f] !== s[f]) { a[f] = s[f]; changed = true; }
                }
            }
            // Only re-render if something actually moved, and not while a modal
            // is open (re-rendering the list under an open detail/add modal
            // would be wasteful; the modal reads its own copy of the row).
            const detailModal = document.getElementById('detailModal');
            const addModal = document.getElementById('addModal');
            const modalOpen = (detailModal && detailModal.classList.contains('active')) ||
                              (addModal && addModal.classList.contains('active'));
            if (changed && !modalOpen) renderAccounts();
        }
        function renderStatusFromObject(d) {
            document.getElementById('statAccounts').textContent = d.accounts || 0;
            document.getElementById('statAvailable').textContent = (d.available !== undefined ? d.available : (d.accounts || 0));
            document.getElementById('statRequests').textContent = d.totalRequests || 0;
            document.getElementById('statSuccess').textContent = d.successRequests || 0;
            document.getElementById('statFailed').textContent = d.failedRequests || 0;
            document.getElementById('statTokens').textContent = formatNum(d.totalTokens || 0);
            const total = d.totalRequests || 0;
            const ok = d.successRequests || 0;
            const rate = total > 0 ? (ok / total * 100) : 0;
            document.getElementById('statSuccessRate').textContent = total > 0 ? rate.toFixed(1) + '%' : '-';
            let limitSum = 0, currentSum = 0;
            if (typeof d.activeQuotaTotal === 'number' && d.activeQuotaTotal > 0) {
                limitSum = d.activeQuotaTotal;
                currentSum = typeof d.activeQuotaUsed === 'number' ? d.activeQuotaUsed : 0;
            } else if (typeof d.quotaTotal === 'number' && d.quotaTotal > 0) {
                limitSum = d.quotaTotal;
                currentSum = typeof d.quotaUsed === 'number' ? d.quotaUsed : 0;
            } else if (Array.isArray(accountsData)) {
                for (const a of accountsData) {
                    if (a.enabled === false) continue;
                    if (typeof a.usageLimit === 'number') limitSum += a.usageLimit;
                    if (typeof a.usageCurrent === 'number') currentSum += a.usageCurrent;
                }
            }
            const usedDisplay = currentSum > 0 ? currentSum : (d.totalCredits || 0);
            document.getElementById('statCredits').textContent = usedDisplay.toFixed(1);
            document.getElementById('statCreditsTotal').textContent = limitSum > 0 ? limitSum.toFixed(0) : '-';
            const remaining = Math.max(0, limitSum - currentSum);
            const remainingEl = document.getElementById('statCreditsRemaining');
            if (limitSum > 0) {
                const pct = (currentSum / limitSum * 100);
                remainingEl.textContent = t('stats.creditsRemaining') + ': ' + remaining.toFixed(0) + ' (' + pct.toFixed(0) + '% used)';
            } else {
                remainingEl.textContent = '';
            }
            const up = d.uptime || 0;
            document.getElementById('statUptime').textContent = formatUptime(up);
        }
        function startDashboardWS() {
            if (!password) return;
            // Idempotent: if a connection is already open or in CONNECTING
            // state, don't open a second. WebSocket states: 0=connecting
            // 1=open 2=closing 3=closed.
            if (dashboardWS && (dashboardWS.readyState === 0 || dashboardWS.readyState === 1)) return;
            try {
                const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
                const url = proto + '//' + location.host + '/admin/ws/status';
                // Subprotocol must be a token (no comma, no whitespace).
                // We pass the admin password after the prefix; the server
                // verifies it with constant-time compare and echoes back a
                // STATIC non-sensitive token ('kiro-admin-v1') to complete the
                // handshake — never the password-bearing token, so the password
                // can't leak into the 101 response header / proxy logs. Both are
                // offered here so the server's echoed choice is one we offered.
                const ws = new WebSocket(url, ['admin-password.' + password, 'kiro-admin-v1']);
                dashboardWS = ws;
                ws.onmessage = (ev) => {
                    if (document.getElementById('mainPage').classList.contains('hidden')) return;
                    try {
                        const data = JSON.parse(ev.data);
                        applyStatusSnapshot(data);
                    } catch (e) { /* ignore malformed frame */ }
                };
                ws.onclose = () => {
                    dashboardWS = null;
                    // Reconnect with backoff. If the server is down or auth
                    // changed, the polling fallback keeps the dashboard alive.
                    if (dashboardWSReconnect) clearTimeout(dashboardWSReconnect);
                    dashboardWSReconnect = setTimeout(startDashboardWS, 5000);
                    ensurePollFallback();
                };
                ws.onerror = () => {
                    // onclose handles cleanup; just trigger fallback so the
                    // dashboard stays current while the WS retries.
                    ensurePollFallback();
                };
            } catch (e) {
                ensurePollFallback();
            }
        }
        function ensurePollFallback() {
            if (dashboardPollFallback) return;
            dashboardPollFallback = setInterval(() => {
                if (!document.getElementById('mainPage').classList.contains('hidden')) loadStats();
            }, 3000);
        }
        // showMain() opens the dashboard WS now that auth is established.
        // The fallback poll runs at a coarser cadence even when the WS is
        // healthy, so any state-change the push path forgets (rare) still
        // surfaces within 10 s.
        setInterval(() => { if (!document.getElementById('mainPage').classList.contains('hidden')) loadStats(); }, 10000);

        // Live-refresh the currently-visible tab whose data is NOT fully carried
        // by the status WebSocket push. The WS push (renderStatusFromObject +
        // mergeAccountStats) keeps the Overview STAT CARDS and the Accounts cards
        // realtime, but three things are fetched per-tab and would otherwise go
        // stale until a manual refresh: the Overview trend sparklines + top-models
        // (loadOverview, history/modelstats endpoints), the API Keys usage, and
        // the Analytics breakdown/trend. A light 5s re-fetch of just the active
        // tab keeps them live without bloating the broadcast snapshot. We skip
        // while a modal is open (a re-render under an open create/detail modal is
        // wasteful) and while the page is hidden (a backgrounded tab shouldn't
        // poll).
        function anyModalOpen() {
            return !!document.querySelector('.modal.active');
        }
        function activeDashboardTab() {
            const el = document.querySelector('.nav-item.active');
            return el ? el.dataset.tab : '';
        }
        setInterval(() => {
            if (document.getElementById('mainPage').classList.contains('hidden')) return;
            if (document.hidden || anyModalOpen()) return;
            const tab = activeDashboardTab();
            if (tab === 'apikeys') loadApiKeys();
            else if (tab === 'analytics') loadAnalytics();
            else if (tab === 'overview') loadOverview();
            else if (tab === 'providers') loadProviders();
        }, 5000);

        // ==================== 版本检查 ====================
        let currentVersion = '';
        async function loadVersion() {
            try {
                const res = await fetch('/admin/api/version', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                currentVersion = d.version || '';
                document.getElementById('versionBadge').textContent = 'v' + currentVersion;
            } catch (e) { }
        }

        async function checkUpdate(manual) {
            try {
                const res = await fetch('https://raw.githubusercontent.com/UntaDotMy/Kiro-Go/main/version.json?t=' + Date.now());
                if (!res.ok) throw new Error('Fetch failed');
                const d = await res.json();
                const latestVersion = (d.version || '').replace(/^v/, '');
                if (latestVersion && latestVersion !== currentVersion && compareVersions(latestVersion, currentVersion) > 0) {
                    showUpdateModal(latestVersion, d.download || 'https://github.com/UntaDotMy/Kiro-Go', d.changelog || '');
                } else if (manual) {
                    alert(t('update.upToDate') + ' (v' + currentVersion + ')');
                }
            } catch (e) {
                if (manual) alert(t('update.checkFailed'));
            }
        }

        // escapeHTML neutralises every server-returned string before innerHTML
        // interpolation. The API key name, account email/userId/region, model
        // ids and any other persisted user-controlled field flows through
        // here. Without it, an admin XSS through e.g. an API key named
        // <img src=x onerror=...> would compromise the dashboard (admin
        // password is in localStorage).
        function escapeHTML(s) {
            if (s == null) return '';
            return String(s)
                .replace(/&/g, '&amp;')
                .replace(/</g, '&lt;')
                .replace(/>/g, '&gt;')
                .replace(/"/g, '&quot;')
                .replace(/'/g, '&#39;');
        }

        function compareVersions(a, b) {
            // Splits "1.0.8-A16" into numeric major.minor.patch and an
            // optional integer suffix from -A##. Pre-A17 used split('.') only,
            // which produced ["1","0","8-A16"] -> Number("8-A16") = NaN, so
            // every -A## release compared as equal and the dashboard's
            // "check update" button never fired.
            const parse = (v) => {
                const m = String(v).match(/^(\d+)\.(\d+)\.(\d+)(?:-A(\d+))?/);
                if (!m) return [0, 0, 0, 0];
                return [Number(m[1]), Number(m[2]), Number(m[3]), Number(m[4] || 0)];
            };
            const pa = parse(a), pb = parse(b);
            for (let i = 0; i < 4; i++) {
                if (pa[i] > pb[i]) return 1;
                if (pa[i] < pb[i]) return -1;
            }
            return 0;
        }

        function showUpdateModal(version, url, changelog) {
            const body = document.getElementById('updateBody');
            // url comes from GitHub release JSON; only honor an https link, else
            // drop to '#' so a hostile/compromised release can't inject a
            // javascript: or data: href. version is escaped before interpolation.
            const safeUrl = (typeof url === 'string' && /^https:\/\//i.test(url)) ? url : '#';
            body.innerHTML =
                '<div style="text-align:center;margin-bottom:20px">' +
                '<div style="font-size:48px;margin-bottom:12px">🎉</div>' +
                '<p style="font-size:16px;font-weight:600;color:#7c3aed">' + t('update.newVersion') + '</p>' +
                '</div>' +
                '<div class="detail-grid" style="margin-bottom:16px">' +
                '<div class="detail-item"><div class="detail-label">' + t('update.current') + '</div><div class="detail-value">v' + currentVersion + '</div></div>' +
                '<div class="detail-item"><div class="detail-label">' + t('update.latest') + '</div><div class="detail-value" style="color:#16a34a">v' + escapeHTML(version) + '</div></div>' +
                '</div>' +
                (changelog ? '<div style="margin-bottom:16px"><div style="font-size:13px;font-weight:600;margin-bottom:8px">' + t('update.changelog') + '</div><div style="background:#f8fafc;padding:12px;border-radius:8px;font-size:12px;max-height:200px;overflow-y:auto;white-space:pre-wrap;line-height:1.6">' + escapeHtml(changelog) + '</div></div>' : '') +
                '<div style="text-align:center"><a href="' + escapeHTML(safeUrl) + '" target="_blank" class="btn btn-primary" style="text-decoration:none;display:inline-block">' + t('update.goDownload') + '</a></div>';
            document.getElementById('updateModal').classList.add('active');
        }

        function closeUpdateModal() { document.getElementById('updateModal').classList.remove('active'); }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        // ==================== 导出功能 ====================
        let exportSelectedIds = new Set();

        function showExportModal() {
            if (accountsData.length === 0) { alert(t('accounts.empty')); return; }
            exportSelectedIds = new Set(accountsData.map(a => a.id));
            renderExportModal();
            document.getElementById('exportModal').classList.add('active');
        }

        function closeExportModal() { document.getElementById('exportModal').classList.remove('active'); }

        function renderExportModal() {
            const body = document.getElementById('exportBody');
            const allSelected = exportSelectedIds.size === accountsData.length;
            body.innerHTML =
                '<div style="margin-bottom:12px;display:flex;justify-content:space-between;align-items:center">' +
                '<span style="font-size:13px;color:#64748b">' + t('export.selected', exportSelectedIds.size) + '</span>' +
                '<button class="btn btn-sm btn-secondary" onclick="toggleExportSelectAll()">' + (allSelected ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
                '</div>' +
                '<div style="max-height:300px;overflow-y:auto;margin-bottom:16px">' +
                accountsData.map(a => {
                    const checked = exportSelectedIds.has(a.id) ? 'checked' : '';
                    return '<label style="display:flex;align-items:center;gap:8px;padding:8px 10px;border-radius:6px;cursor:pointer;margin-bottom:4px;background:' + (exportSelectedIds.has(a.id) ? '#f0f4ff' : '#f8fafc') + '">' +
                        '<input type="checkbox" ' + checked + ' onchange="toggleExportAccount(\'' + a.id + '\')" style="width:16px;height:16px">' +
                        '<div style="flex:1;min-width:0"><div style="font-size:13px;font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + getDisplayEmail(a.email, a.id) + '</div>' +
                        '<div style="font-size:11px;color:#64748b">' + formatAuthMethod(a.provider || a.authMethod) + ' · ' + (a.subscriptionType || 'FREE') + '</div></div>' +
                        '</label>';
                }).join('') +
                '</div>' +
                '<div id="exportJsonPreview" class="hidden" style="margin-bottom:12px"><textarea id="exportJsonText" readonly style="width:100%;min-height:150px;max-height:300px;font-family:monospace;font-size:11px;background:#f8fafc;resize:vertical"></textarea></div>' +
                '<div class="modal-footer" style="flex-wrap:wrap">' +
                '<button class="btn btn-secondary" onclick="closeExportModal()">' + t('common.cancel') + '</button>' +
                '<button class="btn btn-secondary" onclick="exportShowJson()">' + t('export.showJson') + '</button>' +
                '<button class="btn btn-secondary" onclick="exportCopyJson()">' + t('export.copyJson') + '</button>' +
                '<button class="btn btn-secondary" onclick="exportDownloadNineRouter()" title="' + t('export.ninerouterHint') + '">' + t('export.ninerouter') + '</button>' +
                '<button class="btn btn-primary" onclick="exportDownloadJson()">' + t('export.downloadJson') + '</button>' +
                '</div>' +
                '<div style="border-top:1px solid var(--border);margin-top:16px;padding-top:14px">' +
                '<div style="font-size:12px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.4px;margin-bottom:6px">' + t('backup.title') + '</div>' +
                '<div style="font-size:12px;color:var(--muted);margin-bottom:10px">' + t('backup.hint') + '</div>' +
                '<div style="display:flex;gap:8px;flex-wrap:wrap">' +
                '<button class="btn btn-secondary" onclick="downloadFullBackup()">' + t('backup.download') + '</button>' +
                '<button class="btn btn-secondary" onclick="document.getElementById(\'restoreFileInput\').click()">' + t('backup.restore') + '</button>' +
                '<input type="file" id="restoreFileInput" accept="application/json,.json" style="display:none" onchange="restoreFromBackup(this)">' +
                '</div>' +
                '<div id="backupStatus" style="font-size:13px;color:var(--muted);margin-top:8px;min-height:18px"></div>' +
                '</div>';
        }

        function toggleExportAccount(id) {
            if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
            else exportSelectedIds.add(id);
            renderExportModal();
        }

        function toggleExportSelectAll() {
            if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
            else exportSelectedIds = new Set(accountsData.map(a => a.id));
            renderExportModal();
        }

        async function getExportData() {
            if (exportSelectedIds.size === 0) { alert(t('export.noSelection')); return null; }
            const res = await fetch('/admin/api/export', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ ids: Array.from(exportSelectedIds) })
            });
            if (!res.ok) {
                const error = await res.json();
                alert(t('common.failed') + ': ' + (error.error || 'Unknown error'));
                return null;
            }
            return await res.json();
        }

        async function exportShowJson() {
            const data = await getExportData();
            if (!data) return;
            const preview = document.getElementById('exportJsonPreview');
            const textarea = document.getElementById('exportJsonText');
            preview.classList.remove('hidden');
            textarea.value = JSON.stringify(data, null, 2);
        }

        async function exportCopyJson() {
            const data = await getExportData();
            if (!data) return;
            const filtered = data.accounts.map(a => {
                const { clientId, clientSecret, accessToken, refreshToken } = a.credentials || {};
                return { clientId, clientSecret, accessToken, refreshToken };
            });
            await navigator.clipboard.writeText(JSON.stringify(filtered, null, 2));
            alert(t('export.copied'));
        }

        async function exportDownloadJson() {
            const data = await getExportData();
            if (!data) return;
            const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
            a.click();
            URL.revokeObjectURL(url);
        }

        // Export the selected accounts as a 9router logical backup (the shape
        // 9router's GET /api/settings/database returns). Empty selection = all.
        async function exportDownloadNineRouter() {
            if (exportSelectedIds.size === 0) { alert(t('export.noSelection')); return; }
            const res = await fetch('/admin/api/export/9router', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                body: JSON.stringify({ ids: Array.from(exportSelectedIds) })
            });
            if (!res.ok) {
                let msg = 'Unknown error';
                try { msg = (await res.json()).error || msg; } catch (e) { }
                alert(t('common.failed') + ': ' + msg);
                return;
            }
            const text = await res.text();
            const blob = new Blob([text], { type: 'application/json' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = '9router-backup-' + new Date().toISOString().slice(0, 10) + '.json';
            a.click();
            URL.revokeObjectURL(url);
        }

        async function downloadFullBackup() {
            const statusEl = document.getElementById('backupStatus');
            if (statusEl) statusEl.textContent = '';
            const res = await fetch('/admin/api/backup', { headers: { 'X-Admin-Password': password } });
            if (!res.ok) {
                if (statusEl) statusEl.textContent = t('common.failed');
                return;
            }
            const text = await res.text();
            const blob = new Blob([text], { type: 'application/json' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = 'kiro-go-backup-' + new Date().toISOString().slice(0, 10) + '.json';
            a.click();
            URL.revokeObjectURL(url);
        }

        async function restoreFromBackup(input) {
            const file = input.files && input.files[0];
            input.value = '';
            if (!file) return;
            if (!await uiConfirm(t('backup.restoreConfirm'), { danger: true })) return;
            const statusEl = document.getElementById('backupStatus');
            if (statusEl) statusEl.textContent = t('import.running');
            try {
                const text = await file.text();
                const res = await fetch('/admin/api/restore', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: text
                });
                const d = await res.json();
                if (!res.ok || d.error) {
                    if (statusEl) statusEl.textContent = (d.error || t('common.failed'));
                    return;
                }
                if (statusEl) statusEl.textContent = t('backup.restored', d.accounts);
                await loadAccounts();
                if (activeDashboardTab() === 'providers') renderProviders();
                loadStats();
            } catch (e) {
                if (statusEl) statusEl.textContent = t('common.failed');
            }
        }

        // ==================== 导入功能 ====================
        // 解析后的待导入条目，每项 {refreshToken, accessToken?, clientId?, clientSecret?, authMethod?, provider?, region?, email?, nickname?, userId?, machineId?, idx, selected, status?, reason?}
        let importParsedRows = [];

        function showImportModal() {
            importParsedRows = [];
            renderImportModal();
            document.getElementById('importModal').classList.add('active');
        }

        function closeImportModal() {
            document.getElementById('importModal').classList.remove('active');
        }

        function renderImportModal() {
            const body = document.getElementById('importBody');
            const hasRows = importParsedRows.length > 0;
            const selectedCount = importParsedRows.filter(r => r.selected && !r.status).length;
            const totalSelectable = importParsedRows.filter(r => !r.status).length;
            const allSelected = totalSelectable > 0 && selectedCount === totalSelectable;

            let listHtml = '';
            if (hasRows) {
                listHtml =
                    '<div style="margin:12px 0;display:flex;justify-content:space-between;align-items:center">' +
                    '<span style="font-size:13px;color:#64748b">' + t('import.found', importParsedRows.length) + ' · ' + t('import.selected', selectedCount, totalSelectable) + '</span>' +
                    (totalSelectable > 0 ? '<button class="btn btn-sm btn-secondary" onclick="toggleImportSelectAll()">' + (allSelected ? t('import.deselectAll') : t('import.selectAll')) + '</button>' : '') +
                    '</div>' +
                    '<div style="max-height:280px;overflow-y:auto;margin-bottom:16px">' +
                    importParsedRows.map((r, i) => renderImportRow(r, i)).join('') +
                    '</div>';
            }

            body.innerHTML =
                '<p style="font-size:13px;color:#64748b;margin-bottom:12px">' + t('credentials.batchHint') + '</p>' +
                '<div style="display:flex;gap:8px;align-items:center;margin-bottom:8px">' +
                '<input type="file" id="importFileInput" accept=".json,application/json" onchange="importLoadFile(event)" style="font-size:12px">' +
                '<span style="font-size:12px;color:#64748b">' + t('import.orPaste') + '</span>' +
                '</div>' +
                '<div class="form-group"><textarea id="importJsonText" placeholder="' + t('import.parsePlaceholder') + '" style="min-height:140px;font-family:monospace;font-size:11px"></textarea></div>' +
                '<div style="margin-bottom:8px"><button class="btn btn-secondary btn-sm" onclick="importParse()">' + t('import.parse') + '</button>' +
                ' <button class="btn btn-secondary btn-sm" onclick="importNineRouter()" title="' + t('import.ninerouterHint') + '">' + t('import.ninerouter') + '</button></div>' +
                listHtml +
                '<div class="modal-footer">' +
                '<button class="btn btn-secondary" onclick="closeImportModal()">' + t('common.cancel') + '</button>' +
                (hasRows && totalSelectable > 0 ? '<button class="btn btn-primary" id="importRunBtn" onclick="importRun()">' + t('import.run') + '</button>' : '') +
                '</div>';
        }

        function renderImportRow(r, i) {
            // status mirrors the backend response: imported | skipped | failed | invalid
            let badge = '';
            let bg = '#f8fafc';
            let disabled = '';
            if (r.status === 'imported') {
                badge = '<span style="font-size:11px;color:#16a34a;font-weight:600">✓</span>';
                bg = '#f0fdf4';
                disabled = 'disabled';
            } else if (r.status === 'skipped') {
                badge = '<span style="font-size:11px;color:#64748b">' + t('import.skippedRow') + '</span>';
                bg = '#f1f5f9';
                disabled = 'disabled';
            } else if (r.status === 'failed') {
                badge = '<span style="font-size:11px;color:#dc2626">' + t('import.failedRow', escapeHtml(r.reason || '')) + '</span>';
                bg = '#fef2f2';
                disabled = 'disabled';
            } else if (r.status === 'invalid') {
                badge = '<span style="font-size:11px;color:#b45309">' + t('import.invalidRow', escapeHtml(r.reason || '')) + '</span>';
                bg = '#fffbeb';
                disabled = 'disabled';
            }
            const checked = (r.selected && !r.status) ? 'checked' : '';
            const tail = (r.refreshToken || '').slice(-8);
            const display = r.email || (r.userId ? r.userId.slice(0, 12) + '…' : '…' + tail);
            const provider = r.provider || formatAuthMethod(r.authMethod) || '-';
            return '<label style="display:flex;align-items:center;gap:8px;padding:8px 10px;border-radius:6px;margin-bottom:4px;background:' + bg + '">' +
                '<input type="checkbox" ' + checked + ' ' + disabled + ' onchange="toggleImportRow(' + i + ')" style="width:16px;height:16px">' +
                '<div style="flex:1;min-width:0">' +
                '<div style="font-size:13px;font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escapeHtml(display) + '</div>' +
                '<div style="font-size:11px;color:#64748b">' + escapeHtml(provider) + ' · …' + escapeHtml(tail) + '</div>' +
                '</div>' +
                (badge ? '<div style="margin-left:8px">' + badge + '</div>' : '') +
                '</label>';
        }

        function escapeHtml(s) {
            return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
        }

        function importLoadFile(ev) {
            const f = ev.target.files && ev.target.files[0];
            if (!f) return;
            const reader = new FileReader();
            reader.onload = e => {
                document.getElementById('importJsonText').value = String(e.target.result || '');
                importParse();
            };
            reader.readAsText(f);
        }

        function importParse() {
            const raw = (document.getElementById('importJsonText').value || '').trim();
            if (!raw) { importParsedRows = []; renderImportModal(); return; }

            // Three accepted shapes plus a fallback line-by-line refreshToken list.
            let items = [];
            try {
                const j = JSON.parse(raw);
                if (Array.isArray(j)) {
                    items = j;
                } else if (j && Array.isArray(j.accounts)) {
                    items = j.accounts.map(a => {
                        const c = a.credentials || {};
                        return {
                            refreshToken: c.refreshToken || a.refreshToken,
                            accessToken: c.accessToken,
                            clientId: c.clientId || a.clientId,
                            clientSecret: c.clientSecret || a.clientSecret,
                            region: c.region || a.region,
                            authMethod: c.authMethod || a.authMethod,
                            provider: c.provider || a.provider || a.idp,
                            email: a.email,
                            nickname: a.nickname,
                            userId: a.userId,
                            machineId: a.machineId
                        };
                    });
                } else if (j && j.refreshToken) {
                    items = [j];
                }
            } catch (e) {
                // Fallback: maybe one refreshToken per line.
                const lines = raw.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
                if (lines.length > 0 && lines.every(l => !l.startsWith('{') && !l.startsWith('['))) {
                    items = lines.map(l => ({ refreshToken: l }));
                } else {
                    alert(t('import.parseError', e.message || e));
                    return;
                }
            }

            const cleaned = items.filter(it => it && typeof it.refreshToken === 'string' && it.refreshToken.trim() !== '');
            if (cleaned.length === 0) {
                alert(t('import.parseEmpty'));
                importParsedRows = [];
                renderImportModal();
                return;
            }

            importParsedRows = cleaned.map((it, i) => Object.assign({}, it, { idx: i, selected: true, status: '', reason: '' }));
            renderImportModal();
        }

        function toggleImportRow(i) {
            const r = importParsedRows[i];
            if (!r || r.status) return;
            r.selected = !r.selected;
            renderImportModal();
        }

        function toggleImportSelectAll() {
            const selectable = importParsedRows.filter(r => !r.status);
            const allOn = selectable.every(r => r.selected);
            selectable.forEach(r => { r.selected = !allOn; });
            renderImportModal();
        }

        async function importRun() {
            const toSend = importParsedRows.filter(r => r.selected && !r.status);
            if (toSend.length === 0) { alert(t('import.noSelection')); return; }

            const btn = document.getElementById('importRunBtn');
            if (btn) { btn.disabled = true; btn.textContent = t('import.running'); }

            const payload = {
                accounts: toSend.map(r => ({
                    credentials: {
                        refreshToken: r.refreshToken,
                        // Deliberately omit accessToken — the backend always
                        // re-derives a fresh one via auth.RefreshToken.
                        clientId: r.clientId || '',
                        clientSecret: r.clientSecret || '',
                        region: r.region || '',
                        authMethod: r.authMethod || '',
                        provider: r.provider || ''
                    },
                    email: r.email || '',
                    nickname: r.nickname || '',
                    userId: r.userId || '',
                    machineId: r.machineId || '',
                    idp: r.provider || ''
                }))
            };

            let data;
            try {
                const res = await fetch('/admin/api/import', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: JSON.stringify(payload)
                });
                data = await res.json();
                if (!res.ok) {
                    alert(t('import.requestFailed', data && data.error ? data.error : res.status));
                    if (btn) { btn.disabled = false; btn.textContent = t('import.run'); }
                    return;
                }
            } catch (e) {
                alert(t('import.requestFailed', e.message || e));
                if (btn) { btn.disabled = false; btn.textContent = t('import.run'); }
                return;
            }

            // Map per-row results back onto the parsed rows. The server returns
            // results in the same order we sent them, so we walk the selected
            // subset by index.
            const results = (data && data.results) || [];
            const newAccountIds = [];
            toSend.forEach((row, i) => {
                const res = results[i] || {};
                row.status = res.status || 'failed';
                row.reason = res.reason || '';
                if (res.status === 'imported' && res.accountId) {
                    newAccountIds.push(res.accountId);
                }
                if (res.email && !row.email) row.email = res.email;
            });

            renderImportModal();

            const imported = data.imported || 0;
            const skipped = data.skipped || 0;
            const failed = data.failed || 0;
            alert(t('import.done', imported, skipped, failed));

            if (imported > 0) {
                loadAccounts();
                loadStats();
                newAccountIds.forEach(id => autoRefreshNewAccount(id));
            }
        }

        // Import a 9router logical backup (the JSON 9router's
        // GET /api/settings/database returns). Sends the raw textarea/file body
        // to /admin/api/import/9router, which registers custom providers +
        // accounts + inbound keys for Kiro/Codex/Qoder/custom providers.
        async function importNineRouter() {
            const raw = (document.getElementById('importJsonText').value || '').trim();
            if (!raw) { alert(t('import.parseEmpty')); return; }
            // Sanity check: must parse and look like a 9router envelope.
            let probe;
            try { probe = JSON.parse(raw); }
            catch (e) { alert(t('import.parseError', e.message || e)); return; }
            if (!probe || (!Array.isArray(probe.providerConnections) && !Array.isArray(probe.providerNodes) && !Array.isArray(probe.combos))) {
                alert(t('import.ninerouterNotDetected'));
                return;
            }

            let data;
            try {
                const res = await fetch('/admin/api/import/9router', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-Admin-Password': password },
                    body: raw
                });
                data = await res.json();
                if (!res.ok) {
                    alert(t('import.requestFailed', data && data.error ? data.error : res.status));
                    return;
                }
            } catch (e) {
                alert(t('import.requestFailed', e.message || e));
                return;
            }

            const unsupported = (data.unsupportedSlugs || []);
            alert(t('import.ninerouterDone',
                data.accounts || 0, data.providers || 0, data.apiKeys || 0, data.skipped || 0, data.failed || 0)
                + (unsupported.length ? '\n' + t('import.ninerouterUnsupported', unsupported.join(', ')) : ''));

            if ((data.accounts || 0) > 0 || (data.providers || 0) > 0) {
                loadAccounts();
                loadStats();
            }
        }

        // ==================== 添加账号后自动刷新 ====================
        async function autoRefreshNewAccount(accountId) {
            try {
                await fetch('/admin/api/accounts/' + accountId + '/refresh', {
                    method: 'POST', headers: { 'X-Admin-Password': password }
                });
            } catch (e) { }
            loadAccounts();
        }


        // ====== renderAccounts (table) ======
        // Client-side sort state for the accounts table. null = server order.
        let acctSortKey = null, acctSortDir = -1;
        function renderAccounts() {
            const container = document.getElementById('accountsList');
            if (!container) return;
            let filtered = getFilteredAccounts();
            if (filtered.length === 0) {
                container.innerHTML = '<div class="empty"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M19 8v6M22 11h-6"/></svg><div>' + t('accounts.empty') + '</div></div>';
                return;
            }
            if (acctSortKey) {
                filtered = filtered.slice().sort((a, b) => {
                    const va = accountSortValue(a, acctSortKey), vb = accountSortValue(b, acctSortKey);
                    if (va < vb) return -1 * acctSortDir;
                    if (va > vb) return 1 * acctSortDir;
                    return 0;
                });
            }
            const ind = k => acctSortKey === k ? '<span class="sort-ind">' + (acctSortDir < 0 ? '▼' : '▲') + '</span>' : '<span class="sort-ind">⇅</span>';
            const sc = k => acctSortKey === k ? ' sorted' : '';
            const th = (k, label, extra) => '<th class="sortable' + sc(k) + (extra || '') + '" onclick="sortAccounts(\'' + k + '\')">' + label + ind(k) + '</th>';
            const head = '<thead><tr>' +
                '<th style="width:34px"><input type="checkbox" id="selectAllCheckboxTable" onchange="toggleSelectAll(this.checked)" style="width:15px;height:15px;cursor:pointer"></th>' +
                th('email', t('accounts.title')) +
                th('plan', t('detail.subscriptionType')) +
                th('usage', t('accounts.mainQuota')) +
                th('requests', t('accounts.requests'), ' cell-num') +
                th('tokens', t('accounts.tokens'), ' cell-num') +
                th('credits', t('accounts.credits'), ' cell-num') +
                th('expiry', t('accounts.expiry')) +
                th('weight', t('accounts.weight')) +
                '<th style="text-align:right"> </th>' +
                '</tr></thead>';

            const body = filtered.map(a => {
                const usagePercent = (a.usagePercent || 0) * 100;
                const usageClass = usagePercent > 90 ? 'critical' : usagePercent > 70 ? 'high' : '';
                const trialPercent = (a.trialUsagePercent || 0) * 100;
                const trialClass = trialPercent > 90 ? 'critical' : trialPercent > 70 ? 'high' : '';
                const isSelected = selectedAccounts.has(a.id);
                const weightVal = a.weight || 0;
                let rowCls = '';
                if (!a.enabled) rowCls += ' is-disabled';
                if (isSelected) rowCls += ' is-selected';

                const weightBadge = weightVal >= 2 ? '<span class="badge badge-warning">W:' + weightVal + '</span>' : '';
                const overageBadge = a.allowOverage ? '<span class="badge badge-warning">' + t('accounts.overage') + ':' + (a.overageWeight || 1) + '</span>' : '';
                const overQuotaBadge = a.overQuota ? '<span class="badge badge-danger" title="' + t('accounts.overQuotaHint') + '">' + t('accounts.overQuota') + '</span>' : '';
                const cooldownBadge = (a.cooldownSecs > 0) ? '<span class="badge badge-danger" title="' + t('accounts.cooldownHint') + '">' + t('accounts.cooldown') + ' ' + fmtCooldown(a.cooldownSecs) + '</span>' : '';
                const metaBadges = getSubBadge(a.subscriptionType) + getTrialBadge(a) + getStatusBadge(a) + overQuotaBadge + cooldownBadge + weightBadge + overageBadge;

                let usageCell = '<span style="color:var(--faint)">—</span>';
                if (a.usageLimit > 0) {
                    usageCell = '<div class="mini-usage"><div class="usage-bar"><div class="usage-fill ' + usageClass + '" style="width:' + usagePercent + '%"></div></div>' +
                        '<div class="usage-text"><span>' + (a.usageCurrent?.toFixed(1) || 0) + ' / ' + (a.usageLimit?.toFixed(0) || 0) + '</span><span>' + usagePercent.toFixed(0) + '%</span></div></div>';
                }
                if (a.trialUsageLimit > 0) {
                    usageCell += '<div class="mini-usage" style="margin-top:6px"><div class="usage-label" style="font-size:10px">' + t('accounts.trialQuota') + ' ' + formatTrialExpiry(a.trialExpiresAt) + '</div><div class="usage-bar"><div class="usage-fill ' + trialClass + '" style="width:' + trialPercent + '%"></div></div></div>';
                }

                const weightSelect = '<select onchange="quickSetWeight(\'' + a.id + '\',this.value)" class="weight-select">' +
                    [0, 1, 2, 3, 4, 5].map(w => '<option value="' + w + '"' + (weightVal === w ? ' selected' : '') + '>' + w + '</option>').join('') +
                    '</select>';

                const inflight = (a.inflight > 0) ? '<div class="acct-sub num" title="' + t('accounts.inflightHint') + '">' + t('accounts.inflight') + ' ' + a.inflight + '</div>' : '';

                const actions = '<div class="row-actions">' +
                    '<button class="btn btn-sm btn-icon btn-secondary" onclick="refreshAccount(\'' + a.id + '\')" title="' + t('accounts.refresh') + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0114.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0020.49 15"/></svg></button>' +
                    '<button class="btn btn-sm btn-icon btn-secondary" onclick="showDetail(\'' + a.id + '\')" title="' + t('accounts.detail') + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7z"/></svg></button>' +
                    '<button class="btn btn-sm btn-icon btn-secondary" onclick="copyAccountJSON(\'' + a.id + '\', this)" title="' + t('accounts.copyJSON') + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg></button>' +
                    (a.banStatus && a.banStatus !== 'ACTIVE' ? '' :
                        '<button class="btn btn-sm ' + (a.enabled ? 'btn-secondary' : 'btn-primary') + '" onclick="toggleAccount(\'' + a.id + '\',' + !a.enabled + ')">' + (a.enabled ? t('accounts.disable') : t('accounts.enable')) + '</button>') +
                    '<button class="btn btn-sm ' + (a.allowOverage ? 'btn-warning' : 'btn-secondary') + '" onclick="toggleOverage(\'' + a.id + '\',' + !a.allowOverage + ')" title="' + t('accounts.overageToggleHint') + '">' + (a.allowOverage ? t('accounts.overageOn') : t('accounts.overageOff')) + '</button>' +
                    '<button class="btn btn-sm btn-secondary" onclick="testAccount(\'' + a.id + '\')" id="test-' + a.id + '" style="background:#2563eb;color:#fff;border-color:#2563eb">' + t('accounts.test') + '</button>' +
                    '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.id + '\')">' + t('accounts.delete') + '</button>' +
                    '</div>';

                const backend = (a.backend || 'kiro');
                const isProviderAcct = backend !== 'kiro';
                // For Kiro accounts keep the existing IdP-method badge; for a
                // provider account show the backend + how many models it serves.
                const idMeta = isProviderAcct
                    ? '<span class="badge badge-info" title="Upstream provider">' + backend + '</span>' +
                      (a.modelCount ? '<span class="badge badge-success" title="Models fetched from this provider">' + a.modelCount + ' models</span>' : '<span class="badge badge-warning" title="No models cached yet — refresh or send a request">no models</span>')
                    : '<span class="badge badge-info">' + formatAuthMethod(a.provider || a.authMethod) + '</span>';

                return '<tr class="' + rowCls.trim() + '">' +
                    '<td><input type="checkbox" ' + (isSelected ? 'checked' : '') + ' onchange="toggleSelectAccount(\'' + a.id + '\')" style="cursor:pointer;width:15px;height:15px"></td>' +
                    '<td><div class="acct-email">' + getDisplayEmail(a.email || a.nickname, a.id) + '</div><div class="row-meta">' + idMeta + '</div></td>' +
                    '<td><div class="row-meta">' + metaBadges + '</div></td>' +
                    '<td>' + usageCell + '</td>' +
                    '<td class="cell-num">' + (a.requestCount || 0) + '</td>' +
                    '<td class="cell-num">' + formatNum(a.totalTokens || 0) + '</td>' +
                    '<td class="cell-num">' + (a.totalCredits || 0).toFixed(1) + '</td>' +
                    '<td><span class="num">' + formatTokenExpiry(a.expiresAt) + '</span>' + inflight + '</td>' +
                    '<td>' + weightSelect + '</td>' +
                    '<td>' + actions + '</td>' +
                    '</tr>';
            }).join('');

            container.innerHTML = '<div class="table-wrap"><table class="data-table">' + head + '<tbody>' + body + '</tbody></table></div>';

            // Keep both "select all" checkboxes (toolbar + table header) in sync.
            const all = filtered.length > 0 && filtered.every(x => selectedAccounts.has(x.id));
            const cbTop = document.getElementById('selectAllCheckbox');
            const cbTbl = document.getElementById('selectAllCheckboxTable');
            if (cbTop) cbTop.checked = all;
            if (cbTbl) cbTbl.checked = all;
        }

        // ============================================================
        // UI overhaul layer — appended last so these function
        // declarations override the originals in the shared script
        // scope. All data-fetching / auth / websocket logic above is
        // reused unchanged; only presentation is replaced here.
        // ============================================================

        // ---- Theme (auto / light / dark, persisted) ----
        function applyStoredTheme() {
            const saved = localStorage.getItem('kiro_theme');
            if (saved === 'light' || saved === 'dark') {
                document.documentElement.setAttribute('data-theme', saved);
            } else {
                document.documentElement.removeAttribute('data-theme');
            }
        }
        function currentEffectiveTheme() {
            const attr = document.documentElement.getAttribute('data-theme');
            if (attr) return attr;
            return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
        }
        function toggleTheme() {
            const next = currentEffectiveTheme() === 'dark' ? 'light' : 'dark';
            localStorage.setItem('kiro_theme', next);
            document.documentElement.setAttribute('data-theme', next);
            // Repaint charts so stroke colors match the new theme.
            if (!document.getElementById('tabOverview').classList.contains('hidden')) loadOverview();
        }

        // ---- Section navigation (sidebar) ----
        const SECTION_TITLE_KEY = {
            overview: 'nav.overview', accounts: 'tabs.accounts', providers: 'tabs.providers', apikeys: 'tabs.apikeys',
            analytics: 'tabs.analytics', settings: 'tabs.settings', api: 'tabs.api'
        };
        const VALID_TABS = ['overview', 'accounts', 'providers', 'apikeys', 'analytics', 'settings', 'api'];
        // updateHash controls whether switchTab writes location.hash. It's false
        // only when switchTab is itself driven BY a hash change (so we don't
        // recurse through the hashchange listener).
        function switchTab(tab, updateHash) {
            if (VALID_TABS.indexOf(tab) === -1) tab = 'overview';
            document.querySelectorAll('.nav-item').forEach(el => {
                const isActive = el.dataset.tab === tab;
                el.classList.toggle('active', isActive);
                // APG tabs pattern: keep aria-selected in sync and use a roving
                // tabindex so only the active tab is in the Tab order; arrow keys
                // move between the others (see navTabKeydown).
                if (el.getAttribute('role') === 'tab') {
                    el.setAttribute('aria-selected', isActive ? 'true' : 'false');
                    el.tabIndex = isActive ? 0 : -1;
                }
            });
            document.querySelectorAll('.tab-content').forEach(c => c.classList.add('hidden'));
            const target = document.getElementById('tab' + tab.charAt(0).toUpperCase() + tab.slice(1));
            if (target) target.classList.remove('hidden');
            const titleEl = document.getElementById('sectionTitle');
            if (titleEl) { titleEl.setAttribute('data-i18n', SECTION_TITLE_KEY[tab] || 'nav.overview'); titleEl.textContent = t(SECTION_TITLE_KEY[tab] || 'nav.overview'); }
            // Persist the section in the URL hash so a refresh (or a shared link)
            // restores the same section instead of snapping back to Overview.
            if (updateHash !== false && location.hash !== '#' + tab) {
                try { history.replaceState(null, '', '#' + tab); } catch (e) { location.hash = tab; }
            }
            if (tab === 'overview') loadOverview();
            else if (tab === 'providers') loadProviders();
            else if (tab === 'apikeys') loadApiKeys();
            else if (tab === 'analytics') {
                // Ensure the per-key stats source is populated BEFORE rendering.
                // On a fresh login restored to #analytics, apikeysData is empty
                // and loadApiKeys() is async — awaiting it (vs. fire-and-forget)
                // is what stops the key-stats panel rendering blank.
                if (apikeysData.length === 0) { loadApiKeys().then(loadAnalytics); }
                else { loadAnalytics(); }
            }
        }
        // currentHashTab returns the tab named by the URL hash, or '' if none/invalid.
        function currentHashTab() {
            const raw = (location.hash || '').replace(/^#/, '');
            return VALID_TABS.indexOf(raw) !== -1 ? raw : '';
        }
        // navTabKeydown implements the WAI-ARIA APG tabs keyboard pattern for the
        // sidebar tablist: Up/Down (this is a vertical tablist) move focus between
        // tabs; Home/End jump to first/last; Enter/Space activate the focused tab.
        // Arrow movement wraps. We use MANUAL activation (focus moves on arrows but
        // the panel only switches on Enter/Space) because switchTab fires async
        // panel loaders (loadOverview/loadApiKeys/loadAnalytics/...) — auto-
        // activating on every arrow transit would fire 4-5 network bursts in one
        // sweep and risk out-of-order renders. APG prescribes manual activation
        // exactly when revealing a panel is expensive. Mouse onclick still calls
        // switchTab directly, so this is purely additive for keyboard users.
        function navTabKeydown(e) {
            const tabs = Array.from(document.querySelectorAll('.nav[role="tablist"] .nav-item[role="tab"]'));
            if (tabs.length === 0) return;
            const current = document.activeElement;
            let idx = tabs.indexOf(current);
            if (idx === -1) return;
            let next = -1;
            switch (e.key) {
                case 'ArrowDown': case 'ArrowRight': next = (idx + 1) % tabs.length; break;
                case 'ArrowUp': case 'ArrowLeft': next = (idx - 1 + tabs.length) % tabs.length; break;
                case 'Home': next = 0; break;
                case 'End': next = tabs.length - 1; break;
                case 'Enter': case ' ': case 'Spacebar':
                    e.preventDefault();
                    if (current.dataset.tab) switchTab(current.dataset.tab);
                    return;
                default: return;
            }
            if (next !== -1) {
                e.preventDefault();
                // Manual activation: move the roving tabindex + focus only. The
                // visible panel and its loaders do NOT change until Enter/Space.
                // aria-selected is left untouched here (it tracks the *active*
                // tab, which switchTab owns), per the APG manual-activation model.
                const target = tabs[next];
                current.tabIndex = -1;
                target.tabIndex = 0;
                target.focus();
            }
        }
        // React to back/forward and manual hash edits while logged in.
        window.addEventListener('hashchange', function () {
            if (document.getElementById('mainPage').classList.contains('hidden')) return;
            const tab = currentHashTab() || 'overview';
            switchTab(tab, false);
        });

        // ---- Accounts table sorting ----
        function accountSortValue(a, key) {
            switch (key) {
                case 'email': return (a.email || a.id || '').toLowerCase();
                case 'plan': return (a.subscriptionType || '').toLowerCase();
                case 'usage': return (a.usagePercent || 0);
                case 'requests': return (a.requestCount || 0);
                case 'tokens': return (a.totalTokens || 0);
                case 'credits': return (a.totalCredits || 0);
                case 'expiry': return (a.expiresAt || 0);
                case 'weight': return (a.weight || 0);
                default: return 0;
            }
        }
        function sortAccounts(key) {
            if (acctSortKey === key) { acctSortDir *= -1; }
            else { acctSortKey = key; acctSortDir = (key === 'email' || key === 'plan') ? 1 : -1; }
            renderAccounts();
        }

        // ---- API tab: copy-ready code samples ----
        let apiLang = 'curl';
        function setApiLang(lang) {
            apiLang = lang;
            document.querySelectorAll('#apiLangSeg .seg-btn').forEach(b => b.classList.toggle('active', b.dataset.lang === lang));
            renderApiCode();
        }
        function renderApiCode() {
            const pre = document.getElementById('apiCodeSample');
            if (!pre) return;
            const base = baseUrl;
            const model = 'claude-sonnet-4-5';
            const samples = {
                curl:
'curl ' + base + '/v1/messages \\\n' +
'  -H "Content-Type: application/json" \\\n' +
'  -H "anthropic-version: 2023-06-01" \\\n' +
'  -H "Authorization: Bearer sk-kg-..." \\\n' +
'  -d \'{\n' +
'    "model": "' + model + '",\n' +
'    "max_tokens": 1024,\n' +
'    "messages": [{"role": "user", "content": "Hello!"}]\n' +
'  }\'',
                python:
'import anthropic\n\n' +
'client = anthropic.Anthropic(\n' +
'    base_url="' + base + '",\n' +
'    api_key="sk-kg-...",\n' +
')\n\n' +
'msg = client.messages.create(\n' +
'    model="' + model + '",\n' +
'    max_tokens=1024,\n' +
'    messages=[{"role": "user", "content": "Hello!"}],\n' +
')\n' +
'print(msg.content[0].text)',
                node:
"import Anthropic from '@anthropic-ai/sdk';\n\n" +
'const client = new Anthropic({\n' +
"  baseURL: '" + base + "',\n" +
"  apiKey: 'sk-kg-...',\n" +
'});\n\n' +
'const msg = await client.messages.create({\n' +
"  model: '" + model + "',\n" +
'  max_tokens: 1024,\n' +
"  messages: [{ role: 'user', content: 'Hello!' }],\n" +
'});\n' +
'console.log(msg.content[0].text);',
                openai:
'from openai import OpenAI\n\n' +
'client = OpenAI(\n' +
'    base_url="' + base + '/v1",\n' +
'    api_key="sk-kg-...",\n' +
')\n\n' +
'resp = client.chat.completions.create(\n' +
'    model="gpt-4o",  # mapped to a Claude model upstream\n' +
'    messages=[{"role": "user", "content": "Hello!"}],\n' +
')\n' +
'print(resp.choices[0].message.content)'
            };
            pre.textContent = samples[apiLang] || samples.curl;
        }
        function copyCode() {
            const pre = document.getElementById('apiCodeSample');
            if (pre) copyValue(pre.textContent);
        }

        // ---- Settings search: filter the setting cards by their text ----
        function filterSettings() {
            const q = (document.getElementById('settingsSearch').value || '').trim().toLowerCase();
            const tab = document.getElementById('tabSettings');
            if (!tab) return;
            // Each direct .card child is one settings group. Show/hide by whether
            // its visible text contains the query. The search box itself is not a
            // .card, so it's never hidden.
            tab.querySelectorAll(':scope > .card').forEach(card => {
                if (!q) { card.classList.remove('filtered-out'); return; }
                const text = (card.textContent || '').toLowerCase();
                card.classList.toggle('filtered-out', text.indexOf(q) === -1);
            });
        }

        // ---- Sparkline SVG ----
        function sparkline(values, color) {
            const w = 320, h = 72, pad = 4;
            if (!values || values.length === 0) values = [0];
            if (values.length === 1) values = [values[0], values[0]];
            const max = Math.max(...values, 1), min = Math.min(...values, 0);
            const range = (max - min) || 1;
            const n = values.length;
            const x = i => pad + (i * (w - 2 * pad)) / (n - 1);
            const y = v => (h - pad) - ((v - min) / range) * (h - 2 * pad);
            let line = '', area = '';
            values.forEach((v, i) => {
                const px = x(i).toFixed(1), py = y(v).toFixed(1);
                line += (i === 0 ? 'M' : 'L') + px + ' ' + py + ' ';
            });
            area = line + 'L' + x(n - 1).toFixed(1) + ' ' + (h - pad) + ' L' + x(0).toFixed(1) + ' ' + (h - pad) + ' Z';
            const gid = 'g' + Math.random().toString(36).slice(2, 8);
            return '<svg class="chart-svg" viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
                '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
                '<stop offset="0%" stop-color="' + color + '" stop-opacity="0.22"/>' +
                '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
                '<path d="' + area + '" fill="url(#' + gid + ')"/>' +
                '<path d="' + line + '" fill="none" stroke="' + color + '" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>' +
                '</svg>';
        }

        // ---- Overview section ----
        // ---- Realtime Overview chart (uPlot, fed by /admin/api/stats/history) ----
        let ovChart = null;
        let ovRangeDays = 14; // time-range quick-pick state (7 / 14 / 30)
        function setOvRange(days) {
            ovRangeDays = days;
            document.querySelectorAll('#ovRangePick .range-btn').forEach(b => {
                b.classList.toggle('is-active', String(b.dataset.days) === String(days));
            });
            loadOverview(); // re-fetch history for the new window and re-render
        }
        function ovThemeColors() {
            const cs = getComputedStyle(document.documentElement);
            return {
                accent: cs.getPropertyValue('--accent').trim() || '#3d5a99',
                muted: cs.getPropertyValue('--muted').trim() || '#6b7280',
                border: cs.getPropertyValue('--border').trim() || '#e8e9ed',
                surface: cs.getPropertyValue('--surface').trim() || '#ffffff',
            };
        }
        function renderOvMainChart(entries, accent) {
            const host = document.getElementById('ovMainChart');
            if (!host || typeof uPlot === 'undefined') return; // graceful no-op if vendor missing
            const xs = entries.map(e => {
                const d = new Date((e.date || '') + 'T00:00:00Z').getTime() / 1000;
                return Number.isFinite(d) ? d : 0;
            });
            const reqs = entries.map(e => e.requests || 0);
            if (xs.length === 0) { host.innerHTML = ''; if (ovChart) { ovChart.destroy(); ovChart = null; } return; }
            const c = ovThemeColors();
            const stroke = accent || c.accent;
            const data = [xs, reqs];
            const width = Math.max(host.clientWidth || host.offsetWidth || 600, 240);
            const opts = {
                width: width,
                height: 200,
                cursor: { points: { size: 6 } },
                legend: { show: false },
                scales: { x: { time: true } },
                axes: [
                    {
                        stroke: c.muted, grid: { show: false }, ticks: { stroke: c.border, size: 4 },
                        font: '11px ui-monospace, monospace', size: 30,
                        // Explicit M/D formatter — avoids uPlot's default time-tick artifact ("----") on sparse data.
                        values: (u, splits) => splits.map(s => {
                            const d = new Date(s * 1000);
                            return Number.isFinite(d.getTime()) ? (d.getUTCMonth() + 1) + '/' + d.getUTCDate() : '';
                        }),
                    },
                    { stroke: c.muted, grid: { stroke: c.border, width: 1 }, ticks: { show: false }, font: '11px ui-monospace, monospace', size: 44 },
                ],
                series: [
                    {},
                    { label: 'Requests', stroke: stroke, width: 1.5, fill: stroke + '22', points: { show: false } },
                ],
            };
            if (ovChart) { ovChart.destroy(); ovChart = null; }
            host.innerHTML = '';
            ovChart = new uPlot(opts, data, host);
        }
        // Re-fit the chart width on container resize (sidebar collapse, window resize).
        window.addEventListener('resize', () => {
            if (ovChart) {
                const host = document.getElementById('ovMainChart');
                if (host) ovChart.setSize({ width: Math.max(host.clientWidth || 600, 240), height: 200 });
            }
        });
        // Live indicator: flip to "stale" if no status push arrives within 12s.
        let ovLiveTimer = null;
        function pingOvLive() {
            const dot = document.getElementById('ovLiveDot');
            if (!dot) return;
            dot.classList.remove('is-stale');
            if (ovLiveTimer) clearTimeout(ovLiveTimer);
            ovLiveTimer = setTimeout(() => { const d = document.getElementById('ovLiveDot'); if (d) d.classList.add('is-stale'); }, 12000);
        }
        async function loadOverview() {
            const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || '#6366f1';
            const success = getComputedStyle(document.documentElement).getPropertyValue('--success').trim() || '#22c55e';
            // Trend sparklines from persisted daily history.
            const setTxt = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };
            const setHtml = (id, v) => { const el = document.getElementById(id); if (el) el.innerHTML = v; };
            try {
                const res = await fetch('/admin/api/stats/history?scope=global&days=' + ovRangeDays, { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                const entries = (d.entries || []);
                const reqs = entries.map(e => e.requests || 0);
                const creds = entries.map(e => +(e.credits || 0));
                const reqSum = reqs.reduce((a, b) => a + b, 0);
                const credSum = creds.reduce((a, b) => a + b, 0);
                setTxt('ovReqValue', formatNum(reqSum));
                setTxt('ovCredValue', credSum.toFixed(1));
                setHtml('ovReqSpark', sparkline(reqs, accent));
                setHtml('ovCredSpark', sparkline(creds, success));
                renderOvMainChart(entries, accent);
                const first = entries[0]?.date || '', last = entries[entries.length - 1]?.date || '';
                setTxt('ovReqStart', first);
                setTxt('ovReqEnd', last);
                setTxt('ovCredStart', first);
                setTxt('ovCredEnd', last);
            } catch (e) {
                setHtml('ovReqSpark', '<div class="empty" style="padding:18px">' + t('analytics.empty') + '</div>');
                setHtml('ovCredSpark', '');
            }
            // Top models by credits.
            try {
                const res = await fetch('/admin/api/modelstats', { headers: { 'X-Admin-Password': password } });
                const d = await res.json();
                const entries = Object.entries(d.models || {}).sort((a, b) => (b[1].credits || 0) - (a[1].credits || 0)).slice(0, 5);
                const host = document.getElementById('ovTopModels');
                if (entries.length === 0) { host.innerHTML = '<div class="empty" style="padding:18px">' + t('analytics.empty') + '</div>'; }
                else {
                    const maxC = Math.max(...entries.map(e => e[1].credits || 0)) || 1;
                    host.innerHTML = entries.map(([m, s]) => {
                        const pct = ((s.credits || 0) / maxC) * 100;
                        return '<div class="toplist-row"><span class="toplist-name">' + escapeHTML(m) + '</span>' +
                            '<span class="toplist-val">' + (s.requests || 0) + ' req · ' + (s.credits || 0).toFixed(1) + ' cr</span>' +
                            '<div class="usage-bar" style="grid-column:1/3;margin-top:3px"><div class="usage-fill" style="width:' + pct + '%"></div></div></div>';
                    }).join('');
                }
            } catch (e) { }
            // Pool health from the in-memory accounts list + last status snapshot.
            renderOverviewHealth();
        }
        let lastStatusSnapshot = {};
        function renderOverviewHealth() {
            const host = document.getElementById('ovHealth');
            if (!host) return;
            const accts = Array.isArray(accountsData) ? accountsData : [];
            const total = accts.length;
            const enabled = accts.filter(a => a.enabled).length;
            const banned = accts.filter(a => a.banStatus && a.banStatus !== 'ACTIVE').length;
            const noToken = accts.filter(a => !a.hasToken).length;
            const d = lastStatusSnapshot || {};
            const totalReq = d.totalRequests || 0, ok = d.successRequests || 0;
            const rate = totalReq > 0 ? (ok / totalReq * 100) : 0;
            let limitSum = 0, usedSum = 0;
            for (const a of accts) { if (a.enabled) { limitSum += (a.usageLimit || 0); usedSum += (a.usageCurrent || 0); } }
            const quotaPct = limitSum > 0 ? (usedSum / limitSum * 100) : 0;
            const row = (label, val, tone) => '<div class="toplist-row"><span class="toplist-name">' + label + '</span>' +
                '<span class="toplist-val" style="color:' + (tone || 'var(--text)') + '">' + val + '</span></div>';
            host.innerHTML =
                row(t('stats.available'), enabled + ' / ' + total, enabled > 0 ? 'var(--success)' : 'var(--danger)') +
                row(t('stats.successRate'), totalReq > 0 ? rate.toFixed(1) + '%' : '—', rate >= 90 ? 'var(--success)' : rate >= 70 ? 'var(--warning)' : 'var(--danger)') +
                row(t('accounts.banned'), String(banned), banned > 0 ? 'var(--danger)' : 'var(--muted)') +
                row(t('accounts.noToken'), String(noToken), noToken > 0 ? 'var(--warning)' : 'var(--muted)') +
                row(t('stats.creditsTotal'), limitSum > 0 ? usedSum.toFixed(0) + ' / ' + limitSum.toFixed(0) + ' (' + quotaPct.toFixed(0) + '%)' : '—');
        }

        // Capture each status snapshot so the health panel can reflect it.
        (function () {
            const origRender = renderStatusFromObject;
            renderStatusFromObject = function (d) {
                lastStatusSnapshot = d || {};
                origRender(d);
                if (!document.getElementById('tabOverview').classList.contains('hidden')) renderOverviewHealth();
            };
        })();

        // showMain override: keep WS bootstrap, restore the section from the URL
        // hash so a refresh stays on the same page instead of snapping to
        // Overview. Falls back to Overview when there's no (valid) hash.
        (function () {
            const orig = showMain;
            showMain = function () {
                orig();
                applyStoredTheme();
                const tab = currentHashTab() || 'overview';
                switchTab(tab, false);
            };
        })();

        applyStoredTheme();