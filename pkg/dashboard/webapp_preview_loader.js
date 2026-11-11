const frame = document.querySelector('iframe[data-preview-html]');
function send() {
  frame.contentWindow.postMessage({type: 'hermes-preview-document',
    html: frame.dataset.previewHtml, scripts: true}, '*');
}
frame.addEventListener('load', send);
send();
