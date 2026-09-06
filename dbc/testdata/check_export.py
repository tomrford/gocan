"""Optional independent DBC checks, run by TestExport via GOCAN_INTEROP_PYTHON.

Validated with cantools 40.7.1. Expectations specify the fixture's wire meaning;
they do not compare gocan's parser with itself. No checkout code is imported.
"""
import sys
from pathlib import Path

import cantools

file = Path(sys.argv[1])
db = cantools.database.load_file(file, encoding="utf-8", strict=False)
if file.stem == "core":
    msg = db.get_message_by_name("Status")
    values = msg.decode(bytes.fromhex("01 64 00 12 34 00 80 3f"))
    assert str(values["Counter"]) == "Running"
    assert values["Temperature"] == -30
    assert values["BigEndian"] == 0x1234
    assert msg.get_signal_by_name("Ratio").is_float
    assert msg.senders == ["ECU", "Logger"]
    assert msg.cycle_time == 10
    assert msg.signal_groups[0].signal_names == ["Temperature", "BigEndian"]
    fast = db.get_message_by_name("FastStatus")
    assert fast.is_extended_frame and fast.is_fd and fast.frame_id == 0x1ABCDE
    assert fast.dbc.attributes["CANFD_BRS"].value == 1
elif file.stem == "multiplex_j1939":
    msg = db.get_message_by_name("NestedMux")
    values = msg.decode(bytes([2, 4, 17, 8, 23, 0, 0, 0]))
    assert values["Leaf"] == 17 and values["Other"] == 23
    assert msg.get_signal_by_name("ChildSelector").multiplexer_signal == "RootA"
    assert msg.get_signal_by_name("Leaf").multiplexer_ids == [3, 4, 5]
    j1939 = db.get_message_by_name("EngineTemperature")
    assert j1939.is_extended_frame and j1939.protocol == "j1939"
    assert j1939.get_signal_by_name("Coolant").spn == 110
    assert j1939.decode(bytes([80, 0, 0, 0, 0, 0, 0, 0]))["Coolant"] == 40
elif file.stem == "edited":
    msg = db.get_message_by_name("Sample")
    assert msg.decode(bytes([100]))["Value"] == 50
    assert msg.get_signal_by_name("Value").unit == 'a "quoted" unit\\path'
else:
    msg = db.get_message_by_name("Command")
    values = msg.decode(bytes.fromhex("03 9c ff 12 34 80 00 00"))
    assert values["Enable"] == 1 and str(values["Mode"]) == "Torque"
    assert values["Temperature"] == -50  # signed raw -100, scale .1, offset -40
    assert values["BigEndian"] == 0x1234
    assert str(values["SignedCounter"]) == "SNA"  # negative raw choice -128
    assert db.get_message_by_name("FloatStatus").decode(bytes.fromhex("00 00 00 3f"))["Ratio"] == 0.5
