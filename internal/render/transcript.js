(function () {
  var button = document.getElementById('theme-toggle');
  if (button) {
    button.addEventListener('click', function () {
      var light = document.body.classList.toggle('theme-light');
      button.setAttribute('aria-label', light ? 'Use dark theme' : 'Use light theme');
      button.setAttribute('title', light ? 'Use dark theme' : 'Use light theme');
    });
  }
  var menu = document.querySelector('[data-download-menu]');
  if (menu) {
    var trigger = menu.querySelector('[data-download-trigger]');
    var popover = menu.querySelector('[data-download-popover]');
    var options = Array.prototype.slice.call(popover.querySelectorAll('[role="menuitem"]'));
    function closeMenu() { trigger.setAttribute('aria-expanded', 'false'); popover.hidden = true; }
    function openMenu() { trigger.setAttribute('aria-expanded', 'true'); popover.hidden = false; options[0] && options[0].focus(); }
    trigger.addEventListener('click', function () { popover.hidden ? openMenu() : closeMenu(); });
    menu.addEventListener('keydown', function (event) {
      if (event.key === 'Escape') { closeMenu(); trigger.focus(); return; }
      if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
      event.preventDefault();
      if (popover.hidden) {
        trigger.setAttribute('aria-expanded', 'true');
        popover.hidden = false;
        (event.key === 'ArrowDown' ? options[0] : options[options.length - 1]).focus();
        return;
      }
      var index = options.indexOf(document.activeElement);
      index = event.key === 'ArrowDown' ? (index + 1) % options.length : (index - 1 + options.length) % options.length;
      options[index].focus();
    });
    document.addEventListener('click', function (event) { if (!menu.contains(event.target)) closeMenu(); });
  }
  var profileCard = document.getElementById('profile-card');
  var activeProfileTrigger = null;
  var profileData = {};
  try {
    profileData = JSON.parse((document.getElementById('transcript-profiles') || {}).textContent || '{}');
  } catch (error) {}
  function closeProfile(returnFocus) {
    if (profileCard) profileCard.hidden = true;
    if (activeProfileTrigger) {
      activeProfileTrigger.setAttribute('aria-expanded', 'false');
      if (returnFocus) activeProfileTrigger.focus();
    }
    activeProfileTrigger = null;
  }
  function openProfile(trigger) {
    if (!profileCard) return;
    var profile = profileData[trigger.dataset.userId];
    if (!profile) return;
    if (activeProfileTrigger && activeProfileTrigger !== trigger) activeProfileTrigger.setAttribute('aria-expanded', 'false');
    activeProfileTrigger = trigger;
    trigger.setAttribute('aria-expanded', 'true');
    var avatar = profileCard.querySelector('[data-profile-avatar]');
    var bot = profileCard.querySelector('[data-profile-bot]');
    var role = profileCard.querySelector('[data-profile-role]');
    var roleIcon = profileCard.querySelector('[data-profile-role-icon]');
    profileCard.querySelector('[data-profile-name]').textContent = profile.displayName || 'Unknown User';
    profileCard.querySelector('[data-profile-username]').textContent = profile.username ? '@' + profile.username : '';
    if (avatar) { avatar.src = profile.avatar || ''; avatar.alt = profile.displayName || ''; }
    if (bot) bot.hidden = !profile.bot;
    if (role) role.hidden = !profile.roleName;
    if (roleIcon) {
      roleIcon.replaceChildren();
      if (/^(?:https:\\/\\/|\\/)/.test(profile.roleIcon || '')) {
        var roleImage = document.createElement('img');
        roleImage.src = profile.roleIcon;
        roleImage.alt = '';
        roleIcon.appendChild(roleImage);
      } else if (profile.roleEmoji) roleIcon.textContent = profile.roleEmoji;
    }
    profileCard.querySelector('[data-profile-role-name]').textContent = profile.roleName || '';
    profileCard.querySelector('.profile-card-accent').style.background = profile.roleColor || 'var(--brand)';
    profileCard.hidden = false;
    var rect = trigger.getBoundingClientRect();
    var left = Math.min(Math.max(8, rect.left), window.innerWidth - profileCard.offsetWidth - 8);
    var top = rect.bottom + 8;
    if (top + profileCard.offsetHeight > window.innerHeight - 8) top = Math.max(8, rect.top - profileCard.offsetHeight - 8);
    profileCard.style.left = left + 'px';
    profileCard.style.top = top + 'px';
  }
  function closeComponentSelects(except) {
    Array.prototype.forEach.call(document.querySelectorAll('[data-component-select]'), function (select) {
      if (select === except) return;
      var selectTrigger = select.querySelector('[data-component-select-trigger]');
      var selectMenu = select.querySelector('.component-select-menu');
      if (selectTrigger) selectTrigger.setAttribute('aria-expanded', 'false');
      if (selectMenu) selectMenu.hidden = true;
      select.classList.remove('open-up');
    });
  }
  function openComponentSelect(select) {
    var selectTrigger = select.querySelector('[data-component-select-trigger]');
    var selectMenu = select.querySelector('.component-select-menu');
    if (!selectTrigger || !selectMenu || selectTrigger.disabled) return;
    closeComponentSelects(select);
    selectMenu.hidden = false;
    selectTrigger.setAttribute('aria-expanded', 'true');
    select.classList.toggle('open-up', selectMenu.getBoundingClientRect().bottom > window.innerHeight - 8);
  }
  function closeComponentSelect(select) {
    var selectTrigger = select.querySelector('[data-component-select-trigger]');
    var selectMenu = select.querySelector('.component-select-menu');
    if (selectTrigger) selectTrigger.setAttribute('aria-expanded', 'false');
    if (selectMenu) selectMenu.hidden = true;
    select.classList.remove('open-up');
  }
  function updateComponentSelect(select) {
    var chosen = Array.prototype.slice.call(select.querySelectorAll('[data-component-option][aria-selected="true"]'));
    var value = select.querySelector('[data-component-select-value]');
    if (!value) return;
    if (!chosen.length) value.textContent = select.dataset.placeholder || 'Select an option';
    else if (Number(select.dataset.maxValues || 1) === 1) value.textContent = chosen[0].dataset.optionLabel;
    else value.textContent = chosen.length + ' selected';
  }
  document.addEventListener('click', function (event) {
    var archivedButton = event.target.closest('[data-archived-action]');
    if (archivedButton) {
      archivedButton.setAttribute('data-action-feedback', 'true');
      clearTimeout(archivedButton._feedbackTimer);
      archivedButton._feedbackTimer = setTimeout(function () { archivedButton.removeAttribute('data-action-feedback'); }, 1800);
      return;
    }
    var option = event.target.closest('[data-component-option]');
    if (option) {
      var optionSelect = option.closest('[data-component-select]');
      var maxValues = Number(optionSelect.dataset.maxValues || 1);
      var selected = option.getAttribute('aria-selected') === 'true';
      if (maxValues === 1) {
        Array.prototype.forEach.call(optionSelect.querySelectorAll('[data-component-option]'), function (item) { item.setAttribute('aria-selected', 'false'); });
        option.setAttribute('aria-selected', 'true');
        updateComponentSelect(optionSelect);
        closeComponentSelect(optionSelect);
      } else {
        var selectedCount = optionSelect.querySelectorAll('[data-component-option][aria-selected="true"]').length;
        if (selected || selectedCount < maxValues) option.setAttribute('aria-selected', selected ? 'false' : 'true');
        updateComponentSelect(optionSelect);
      }
      return;
    }
    var selectTrigger = event.target.closest('[data-component-select-trigger]');
    if (selectTrigger) {
      var select = selectTrigger.closest('[data-component-select]');
      if (selectTrigger.getAttribute('aria-expanded') === 'true') closeComponentSelect(select);
      else openComponentSelect(select);
      return;
    }
    closeComponentSelects();
  });
  document.addEventListener('keydown', function (event) {
    var profileTrigger = event.target.closest && event.target.closest('[data-user-id]');
    if ((event.key === 'Enter' || event.key === ' ') && profileTrigger) { event.preventDefault(); openProfile(profileTrigger); return; }
    if (event.key === 'Escape' && profileCard && !profileCard.hidden) { closeProfile(true); return; }
    var select = event.target.closest && event.target.closest('[data-component-select]');
    if (!select) return;
    var selectTrigger = select.querySelector('[data-component-select-trigger]');
    var options = Array.prototype.slice.call(select.querySelectorAll('[data-component-option]'));
    if (event.key === 'Escape') { closeComponentSelect(select); selectTrigger.focus(); return; }
    if (!['ArrowDown', 'ArrowUp'].includes(event.key) || !options.length) return;
    event.preventDefault();
    if (selectTrigger.getAttribute('aria-expanded') !== 'true') openComponentSelect(select);
    var current = options.indexOf(document.activeElement);
    var next = event.key === 'ArrowDown' ? (current + 1) % options.length : (current - 1 + options.length) % options.length;
    options[next].focus();
  });
  document.addEventListener('click', function (event) {
    var profileTrigger = event.target.closest('[data-user-id]');
    if (profileTrigger) {
      openProfile(profileTrigger);
      return;
    }
    if (profileCard && !profileCard.hidden && !profileCard.contains(event.target)) closeProfile(false);
    var spoilerReveal = event.target.closest('[data-spoiler-reveal]');
    if (spoilerReveal) {
      event.preventDefault();
      var spoiler = spoilerReveal.closest('[data-spoiler-container]');
      var spoilerContent = spoiler && spoiler.querySelector('[data-spoiler-content]');
      if (spoiler) spoiler.classList.add('revealed');
      if (spoilerContent) spoilerContent.removeAttribute('inert');
      spoilerReveal.hidden = true;
      var revealedControl = spoilerContent && spoilerContent.querySelector('a[href], button:not([disabled]), audio[controls], video[controls]');
      if (revealedControl) revealedControl.focus();
      else if (spoilerContent) spoilerContent.focus();
      return;
    }
    var mediaTrigger = event.target.closest('[data-lightbox-src]');
    if (mediaTrigger) {
      var lightbox = document.getElementById('image-lightbox');
      var lightboxImage = lightbox && lightbox.querySelector('[data-lightbox-image]');
      var lightboxCaption = lightbox && lightbox.querySelector('[data-lightbox-caption]');
      if (lightbox && lightboxImage) {
        lightboxImage.src = mediaTrigger.dataset.lightboxSrc;
        lightboxImage.alt = mediaTrigger.dataset.lightboxAlt || '';
        if (lightboxCaption) lightboxCaption.textContent = mediaTrigger.dataset.lightboxCaption || '';
        document.body.classList.add('lightbox-open');
        lightbox.showModal();
      }
      return;
    }
    var spoiler = event.target.closest('.md-spoiler');
    if (spoiler) spoiler.classList.toggle('revealed');
    var linkButton = event.target.closest('[data-copy-message-link]');
    if (!linkButton) return;
    var link = location.href.split('#')[0] + '#' + linkButton.dataset.copyMessageLink;
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(link);
    else {
      var field = document.createElement('textarea'); field.value = link; document.body.appendChild(field); field.select(); document.execCommand('copy'); field.remove();
    }
    var oldLabel = linkButton.getAttribute('aria-label');
    linkButton.setAttribute('aria-label', 'Message link copied');
    setTimeout(function () { linkButton.setAttribute('aria-label', oldLabel); }, 1500);
  });
  var lightbox = document.getElementById('image-lightbox');
  if (lightbox) {
    var closeButton = lightbox.querySelector('[data-lightbox-close]');
    if (closeButton) closeButton.addEventListener('click', function () { lightbox.close(); });
    lightbox.addEventListener('click', function (event) { if (event.target === lightbox) lightbox.close(); });
    lightbox.addEventListener('close', function () {
      document.body.classList.remove('lightbox-open');
      var image = lightbox.querySelector('[data-lightbox-image]');
      if (image) image.removeAttribute('src');
    });
  }
  var transcriptEnd = document.getElementById('transcript-end');
  if (transcriptEnd && !location.hash) {
    if ('scrollRestoration' in history) history.scrollRestoration = 'manual';
    var followNewest = true;
    function showNewestMessage() { if (followNewest) transcriptEnd.scrollIntoView({ block: 'end' }); }
    function stopFollowingNewest() { followNewest = false; }
    ['wheel', 'touchstart', 'pointerdown', 'keydown'].forEach(function (type) {
      window.addEventListener(type, stopFollowingNewest, { once: true, passive: true });
    });
    Array.prototype.forEach.call(document.images, function (image) {
      if (!image.complete) {
        image.addEventListener('load', showNewestMessage, { once: true });
        image.addEventListener('error', showNewestMessage, { once: true });
      }
    });
    requestAnimationFrame(showNewestMessage);
    window.addEventListener('load', showNewestMessage, { once: true });
    setTimeout(showNewestMessage, 250);
    setTimeout(function () { showNewestMessage(); followNewest = false; }, 1500);
  }
