/**
 * YT下载器 - 前端交互逻辑（v12.0.1）
 *
 * 核心流程：
 *   1. 用户输入 URL，点击下载
 *   2. 前端调用 /api/parse 解析链接
 *   3. 若 direct_url 可用 → 客户端直接下载（不经过服务器，节省带宽）
 *   4. 若 fallback_required=true → 服务器代理下载文件流
 *
 * 交互状态流转：
 *   空闲 → 解析中 → 下载中（持续显示，不自动重置）→ 完成/失败
 *
 * 设计原则：
 *   - 不修改 index.html 的任何结构与样式
 *   - 4K 大文件下载使用浏览器原生下载管理器，内存占用最低
 *   - 服务器代理时用 io.Copy 流式推送，服务端内存恒定
 *   - 不用 setTimeout 猜测状态，避免大文件下载被误判为超时
 */
(function () {
  'use strict';

  document.addEventListener('DOMContentLoaded', function () {
    var inputEl = document.querySelector('input[type="text"]');
    var buttonEl = document.querySelector('.download-btn');
    var driverEl = document.querySelector('.driver-text');

    if (!inputEl || !buttonEl) {
      console.error('未找到输入框或下载按钮，请检查 index.html 结构');
      return;
    }

    var originalBtnText = buttonEl.textContent;
    var originalDriverText = driverEl ? driverEl.textContent : '';
    var isDownloading = false;

    // ====== 在输入框末尾注入清空按钮（不修改 HTML 结构） ======
    var wrapper = document.createElement('div');
    wrapper.style.cssText = 'position:relative;display:inline-block;';
    inputEl.parentNode.insertBefore(wrapper, inputEl);
    wrapper.appendChild(inputEl);

    var clearBtn = document.createElement('button');
    clearBtn.type = 'button';
    clearBtn.setAttribute('aria-label', '清空输入');
    clearBtn.textContent = '×';
    clearBtn.style.cssText = [
      'position:absolute',
      'right:8px',
      'top:50%',
      'transform:translateY(-50%)',
      'width:20px',
      'height:20px',
      'line-height:18px',
      'padding:0',
      'border:none',
      'border-radius:50%',
      'background:rgba(255,135,161,0.65)',
      'color:#fff',
      'font-size:14px',
      'font-weight:bold',
      'cursor:pointer',
      'display:none',
      'z-index:5',
      '-webkit-tap-highlight-color:transparent'
    ].join(';');
    wrapper.appendChild(clearBtn);

    inputEl.style.paddingRight = '32px';

    function updateClearBtn() {
      clearBtn.style.display = inputEl.value ? 'block' : 'none';
    }
    inputEl.addEventListener('input', updateClearBtn);
    inputEl.addEventListener('change', updateClearBtn);
    inputEl.addEventListener('keyup', updateClearBtn);
    updateClearBtn();

    clearBtn.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      inputEl.value = '';
      updateClearBtn();
      inputEl.focus();
    });

    /**
     * 从输入中提取真实 URL
     * 支持"【标题】 https://b23.tv/xxx"、"xxx https://b23.tv/xxx 复制此链接"等格式
     */
    function extractUrl(input) {
      if (!input) return '';
      input = String(input).trim();
      var idx = input.indexOf('http://');
      if (idx < 0) idx = input.indexOf('https://');
      if (idx < 0) return '';
      var rest = input.substring(idx);
      var end = rest.search(/[\s\u3000]/);
      if (end >= 0) rest = rest.substring(0, end);
      return rest;
    }

    /**
     * 设置状态文字（按钮 + 底部说明）
     */
    function setStatus(btnText, driverText) {
      if (btnText !== null && btnText !== undefined) buttonEl.textContent = btnText;
      if (driverEl && driverText !== null && driverText !== undefined) {
        driverEl.textContent = driverText;
        driverEl.style.color = '';
      }
    }

    /**
     * 重置为空闲状态
     */
    function resetButton() {
      isDownloading = false;
      buttonEl.disabled = false;
      buttonEl.textContent = originalBtnText;
      if (driverEl) {
        driverEl.textContent = originalDriverText;
        driverEl.style.color = '';
      }
    }

    /**
     * 触发下载主流程
     * 1. 调用 /api/parse 解析
     * 2. 根据解析结果选择直连或代理
     */
    function triggerDownload(url) {
      if (isDownloading) return;
      isDownloading = true;
      buttonEl.disabled = true;
      setStatus('解析中...', '正在调用 yt-dlp 解析链接，请稍候...');

      fetch('/api/parse', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: url })
      })
        .then(function (res) { return res.json(); })
        .then(function (data) {
          if (!data.success) {
            // 解析失败，显示错误，不弹窗
            setStatus(originalBtnText, '❌ ' + (data.error || '解析失败'));
            if (driverEl) driverEl.style.color = '#ff6b8a';
            setTimeout(resetButton, 4000);
            return;
          }

          // 解析成功，显示视频信息
          var info = (data.title || '未知标题') + ' · ' + (data.format || '');

          if (data.fallback_required) {
            // 需要服务器代理下载（分离流合并 / HLS / DASH）
            proxyDownload(url, data, info);
          } else if (data.direct_url) {
            // 可直连下载，客户端直接请求媒体 URL
            directDownload(data, info);
          } else {
            // 异常情况：无直链且无需代理，尝试代理下载
            proxyDownload(url, data, info);
          }
        })
        .catch(function (err) {
          setStatus(originalBtnText, '❌ 解析请求失败：' + err.message);
          if (driverEl) driverEl.style.color = '#ff6b8a';
          setTimeout(resetButton, 4000);
        });
    }

    /**
     * 直连下载：用隐藏 iframe 导航到媒体直链，触发浏览器原生下载
     * （fetch/XHR 无法触发浏览器下载管理器，必须用导航方式）
     */
    function directDownload(data, info) {
      setStatus('下载中...', '📥 ' + info);
      var frame = document.getElementsByName('ytdlp-download-frame')[0];
      if (!frame) {
        frame = document.createElement('iframe');
        frame.name = 'ytdlp-download-frame';
        frame.style.display = 'none';
        document.body.appendChild(frame);
      }
      // 直接用 iframe 导航到媒体 URL，触发下载
      frame.src = data.direct_url;

      // 直连下载由浏览器处理，前端无法精确检测完成时机
      // 显示"下载已开始"提示，3秒后恢复按钮（用户可在浏览器下载管理器查看进度）
      setTimeout(function () {
        setStatus(originalBtnText, '✅ 下载已开始，请查看浏览器下载管理器');
        if (driverEl) driverEl.style.color = '#2e7d32';
        setTimeout(resetButton, 3000);
      }, 2000);
    }

    /**
     * 代理下载：用隐藏表单提交到 /api/download，服务器下载合并后返回文件流
     *
     * 关键改进（v1.0.4）：
     *   - 不用 setTimeout 猜测完成时机（大文件会误判超时）
     *   - 持续显示"下载中..."状态，直到浏览器真正开始接收文件流
     *   - 浏览器收到 Content-Disposition 头后会自动触发下载管理器
     *   - 用表单提交 + iframe 方式，浏览器原生处理文件流响应
     */
    function proxyDownload(url, data, info) {
      setStatus('下载中...', '📥 ' + info + '（服务器正在下载并合并，大文件请耐心等待）');

      var frame = document.getElementsByName('ytdlp-download-frame')[0];
      if (!frame) {
        frame = document.createElement('iframe');
        frame.name = 'ytdlp-download-frame';
        frame.style.display = 'none';
        document.body.appendChild(frame);
      }

      // 监听 iframe 加载，检测错误响应
      // 注意：成功下载时 iframe 内容为空（浏览器直接触发文件下载），onload 不会触发
      // 只有返回错误（如 HTML 错误页）时 onload 才会触发
      frame.onload = function () {
        try {
          var doc = frame.contentDocument || frame.contentWindow.document;
          var body = doc.body ? doc.body.innerText : '';
          // 若 iframe 有内容且不是空响应，说明是错误
          if (body && body.length > 0 && body.length < 2000) {
            setStatus(originalBtnText, '❌ 下载失败：' + body.substring(0, 200));
            if (driverEl) driverEl.style.color = '#ff6b8a';
            setTimeout(resetButton, 5000);
          }
        } catch (e) {
          // 跨域无法读取，忽略（通常是正常下载）
        }
      };

      // 创建隐藏表单提交
      var form = document.createElement('form');
      form.method = 'POST';
      form.action = '/api/download';
      form.target = 'ytdlp-download-frame';
      form.style.display = 'none';
      form.enctype = 'application/x-www-form-urlencoded';

      var urlInput = document.createElement('input');
      urlInput.type = 'hidden';
      urlInput.name = 'url';
      urlInput.value = url;
      form.appendChild(urlInput);

      document.body.appendChild(form);
      form.submit();
      document.body.removeChild(form);

      // 提交后持续显示"下载中"状态
      // 浏览器收到文件流响应头后会自动弹出下载，用户在下载管理器看进度
      // 前端保持"下载中"状态 60 秒（足够服务器开始推送），
      // 之后提示"下载已开始"并恢复按钮（但服务器可能仍在推送，不影响浏览器下载）
      setStatus('下载中...', '📥 ' + info + '（正在传输，请查看浏览器下载管理器）');

      // 60 秒后切换为"下载已开始"提示（不再自动重置为空闲，让用户知道在下载）
      setTimeout(function () {
        if (isDownloading) {
          setStatus(originalBtnText, '✅ 下载进行中，请查看浏览器下载管理器');
          if (driverEl) driverEl.style.color = '#2e7d32';
          resetButton();
        }
      }, 60000);
    }

    /**
     * 点击下载按钮 —— 无感提取 URL，不弹窗
     */
    buttonEl.addEventListener('click', function () {
      var raw = inputEl.value.trim();
      if (!raw) {
        if (driverEl) {
          driverEl.textContent = '请先输入视频或音频链接';
          driverEl.style.color = '#ff6b8a';
          setTimeout(function () {
            driverEl.textContent = originalDriverText;
            driverEl.style.color = '';
          }, 2000);
        }
        inputEl.focus();
        return;
      }
      var url = extractUrl(raw);
      if (!url) {
        if (driverEl) {
          driverEl.textContent = '未找到有效链接，请输入包含 http:// 或 https:// 的链接';
          driverEl.style.color = '#ff6b8a';
          setTimeout(function () {
            driverEl.textContent = originalDriverText;
            driverEl.style.color = '';
          }, 3000);
        }
        inputEl.focus();
        return;
      }
      if (url !== raw) {
        inputEl.value = url;
      }
      triggerDownload(url);
    });

    /**
     * 回车键提交
     */
    inputEl.addEventListener('keypress', function (e) {
      if (e.key === 'Enter' || e.keyCode === 13) {
        e.preventDefault();
        buttonEl.click();
      }
    });

    /**
     * 粘贴时自动去空格
     */
    inputEl.addEventListener('paste', function (e) {
      setTimeout(function () {
        inputEl.value = inputEl.value.trim();
      }, 0);
    });

    // ====== 强制固定页面，禁止任何滚动（含键盘弹出场景） ======
    (function lockPage() {
      document.addEventListener('touchmove', function (e) {
        if (e.target === inputEl) return;
        e.preventDefault();
      }, { passive: false });

      document.addEventListener('wheel', function (e) {
        e.preventDefault();
      }, { passive: false });

      window.addEventListener('scroll', function () {
        window.scrollTo(0, 0);
      }, { passive: true });

      window.addEventListener('resize', function () {
        window.scrollTo(0, 0);
        document.documentElement.scrollTop = 0;
        document.body.scrollTop = 0;
      });

      inputEl.addEventListener('blur', function () {
        setTimeout(function () {
          window.scrollTo(0, 0);
          document.documentElement.scrollTop = 0;
          document.body.scrollTop = 0;
        }, 100);
      });

      window.scrollTo(0, 0);
    })();

    // ====== 在页面最底部显示版本号 ======
    var APP_VERSION = 'v12.0.1';
    (function showVersion() {
      var versionEl = document.createElement('div');
      versionEl.id = 'app-version';
      versionEl.textContent = APP_VERSION;
      versionEl.style.cssText = [
        'position:fixed',
        'bottom:4px',
        'left:50%',
        'transform:translateX(-50%)',
        'font-size:11px',
        'color:rgba(255,135,161,0.7)',
        'font-family:XianSheng,sans-serif',
        'z-index:100',
        'pointer-events:none',
        'text-shadow:0 0 2px rgba(255,255,255,0.8)'
      ].join(';');
      document.body.appendChild(versionEl);
    })();

    console.log('%cYT下载器已就绪 ' + APP_VERSION, 'color:#ff87a1;font-size:14px;font-weight:bold;');
    console.log('%c由 yt-dlp 驱动 · 支持 4K120帧 HDR / 8K · 智能直连/代理', 'color:#34bdc8;');
  });
})();
