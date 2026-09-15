// A fragment is not sent to the server, proxy access logs or Referer headers.
const emailToken = document.getElementById('email-token');
if (emailToken && window.location.hash) emailToken.value = window.location.hash.slice(1);
