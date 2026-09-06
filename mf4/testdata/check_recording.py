"""Independent optional oracle for TestRecording (asammdf 8.7.2/python-can 4.6.1)."""
import sys
import xml.etree.ElementTree as ET

import can
import numpy as np
from asammdf import MDF

filename = sys.argv[1]
with MDF(filename, process_bus_logging=False) as mdf:
    assert mdf.version == "4.10"
    assert mdf.header.abs_time == 1788688800123456789
    assert mdf.identification.unfinalized_standard_flags == 0
    assert len(mdf.file_history) == 1
    history = ET.fromstring(mdf.file_history[0].comment)
    assert history.find("{http://www.asam.net/mdf/v4}tool_id").text == "gocan"
    assert len(mdf.groups) == 4
    assert [g.channel_group.cycles_nr for g in mdf.groups] == [802, 1, 1, 1]
    data = mdf.get("CAN_DataFrame", group=0)
    assert data.samples["CAN_DataFrame.BusChannel"].tolist() == [1] * 802
    assert data.samples["CAN_DataFrame.ID"].tolist() == [0x123] * 802
    assert data.timestamps[0] == 0 and data.timestamps[-1] == 0.803
    assert data.timestamps[-2] == 0.802
    payload = data.samples["CAN_DataFrame.DataBytes"]
    assert payload[0, :8].tolist() == [40, 2, 3, 4, 5, 6, 7, 8]
    assert payload[-1].tolist() == [44] + [0] * 63
    fd = mdf.get("CAN_DataFrame", group=1).samples[0]
    for name, expected in {"BusChannel": 300, "ID": 0x1ABCDE, "IDE": 1,
                           "DLC": 15, "DataLength": 64, "Dir": 1, "EDL": 1,
                           "BRS": 1, "ESI": 1}.items():
        assert fd["CAN_DataFrame." + name] == expected, name
    assert fd["CAN_DataFrame.DataBytes"].tolist() == list(range(64))
    remote = mdf.get("CAN_RemoteFrame", group=2).samples[0]
    assert remote["CAN_RemoteFrame.ID"] == 0x456
    assert remote["CAN_RemoteFrame.DLC"] == 15
    assert remote["CAN_RemoteFrame.Dir"] == 1
    assert len(mdf.attachments) == 2
    assert mdf.attachments[0].file_name == "bus1.dbc"
    assert b"BO_ 291 Status" in mdf.attachments[0].extract()
    structures = [next(c for c in g.channels if c.name in ("CAN_DataFrame", "CAN_RemoteFrame")) for g in mdf.groups]
    assert [c.attachment for c in structures] == [0, None, 0, 1]
    for group in mdf.groups:
        for channel in group.channels:
            if channel.name.startswith(("CAN_DataFrame.", "CAN_RemoteFrame.")):
                assert channel.source_addr == 0  # components inherit their root's source

# python-can interprets the standard structure as CAN messages. Its API reports
# data length, not the original wire DLC, so check wire DLC above with asammdf.
messages = list(can.MF4Reader(filename))
assert len(messages) == 805
fd = next(m for m in messages if m.channel == 300)
assert fd.is_fd and fd.is_extended_id and not fd.is_rx
assert fd.bitrate_switch and fd.error_state_indicator and fd.dlc == 64
assert fd.data == bytes(range(64))
remote = next(m for m in messages if m.is_remote_frame)
assert remote.channel == 1 and remote.arbitration_id == 0x456 and not remote.is_rx

# Also exercise automatic DBC association, not merely attachment extraction.
with MDF(filename) as mdf:
    # asammdf 8.7.2 registers the same alias twice; select its actual channel
    # explicitly after checking that both aliases resolve to that one channel.
    locations = set(mdf.whereis("CAN1.Status.Value"))
    assert len(locations) == 1
    group, index = locations.pop()
    signal = mdf.get(group=group, index=index)
    assert len(signal.samples) == 802
    np.testing.assert_array_equal(signal.samples, [10] + [11] * 800 + [12])
    locations = set(mdf.whereis("CAN2.Second.Value"))
    assert len(locations) == 1
    group, index = locations.pop()
    np.testing.assert_array_equal(mdf.get(group=group, index=index).samples, [120])
