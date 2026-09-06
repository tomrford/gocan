// Optional TestRecording oracle against a built cantraceviewer /direct entry.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

const { createDirectClient, wasmUrl } = await import(pathToFileURL(process.argv[2]));
const client = createDirectClient(readFileSync(wasmUrl));
try {
  const trace = client.openTrace('mf4', readFileSync(process.argv[3]));
  assert.equal(trace.metadata.validMessageCount, 804);
  assert.equal(trace.metadata.durationNs, 803000000);
  assert.equal(trace.hasRawFrames, true);
  assert.deepEqual(trace.warnings, []);
  assert.equal(trace.embeddedDbcs.length, 2);
  assert.equal(trace.embeddedDbcs[0].name, 'bus1.dbc');
  const dbc = client.openDbc(trace.embeddedDbcs[0].text);
  const series = client.getSignalValues(dbc.handle, trace.handle, dbc.catalog.messages[0], 'Value');
  // The existing viewer matches the database message's payload size (8).
  assert.deepEqual([...series.values], [10]);
  const secondDbc = client.openDbc(trace.embeddedDbcs[1].text);
  const second = client.getSignalValues(secondDbc.handle, trace.handle, secondDbc.catalog.messages[0], 'Value');
  assert.deepEqual([...second.values], [120]);
} finally {
  client.close();
}
