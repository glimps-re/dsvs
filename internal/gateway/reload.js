// Reloads the page when what it saw changes: each data-* of the script names a fact of the dsvs
// stream — apps, where the address's apps stand, or config, the applied configuration — with the
// value it had when the page was served. The stream tells each fact on opening then on each change,
// and the comparison, rather than a mere signal, catches up on what changed between the page and
// the stream.
{
  const seen = document.currentScript.dataset;
  const events = new EventSource("/__dsvs/events");
  for (const [fact, value] of Object.entries(seen)) {
    events.addEventListener(fact, (event) => {
      if (event.data !== value) location.reload();
    });
  }
}
