"""Compare the fixed Windows test output with a pre-render plan. Never open the source drive."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import re
import subprocess
import sys

BASE = Path("C:/Archive/scratch/source-classification-rehearsal")
OUTPUT = BASE / "output"


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8-sig"))


def check(condition, message):
    if not condition:
        raise ValueError(message)


def entries(directory):
    children = list(directory.iterdir())
    for child in children:
        check(not child.is_symlink() and not child.is_junction(), f"Linked entry: {child.name}")
    return {child.name: child for child in children}


def segment_count(path, duration):
    content = path.read_text(encoding="utf-8-sig").strip()
    if not content:
        return 0
    previous = 0
    segments = re.split(r"\n\s*\n", content)
    for number, block in enumerate(segments, 1):
        lines = block.splitlines()
        check(len(lines) >= 3 and lines[0] == str(number), f"Invalid SRT block {number}")
        bounds = lines[1].split(" --> ")
        check(len(bounds) == 2, f"Missing SRT range in block {number}")
        times = []
        for bound in bounds:
            match = re.fullmatch(r"(\d+):(\d\d):(\d\d),(\d{3})", bound)
            check(match is not None, f"Invalid SRT timestamp in block {number}")
            h, m, s, ms = map(int, match.groups())
            times.append(((h * 60 + m) * 60 + s) * 1000 + ms)
        check(previous <= times[0] <= times[1] <= duration * 1000 + 1000, f"SRT outside timeline in block {number}")
        previous = times[0]
    return len(segments)


def verify_video(folder, item, run_id, revision, probe):
    stem = item["stem"]
    path = folder / (stem + ".mp4")
    metadata = read_json(folder / (stem + ".json"))
    check(metadata["run_id"] == run_id and metadata["input_revision"] == revision, "Wrong run or source revision")
    check(metadata["source_event"] == folder.name and metadata["video"] == path.name, "Incorrect event or video name")
    check(metadata["source_paths"] == item["sources"], "Source list or clip order changed")
    with path.open("rb") as video:
        checksum = hashlib.file_digest(video, "sha256").hexdigest()
    check(checksum == metadata["video_sha256"], "Video hash differs from metadata")
    result = subprocess.run(
        [str(probe), "-v", "error", "-show_streams", "-show_format", "-show_chapters", "-of", "json", str(path)],
        capture_output=True, timeout=60, check=True,
    )
    inspected = json.loads(result.stdout)
    video = next(stream for stream in inspected["streams"] if stream["codec_type"] == "video")
    check((video["codec_name"], video["width"], video["height"]) == ("h264", 1920, 1080), "Unexpected video format")
    bitrate = int(video["bit_rate"])
    check(abs(bitrate / (item["mbps"] * 1_000_000) - 1) <= 0.15, "Video bitrate outside profile tolerance")
    duration = float(inspected["format"]["duration"])
    check(math.isclose(duration, item["duration_s"], rel_tol=0.001, abs_tol=1.0), "Incorrect rendered duration")
    markers = [round(float(chapter["start_time"]) * 1000) for chapter in inspected.get("chapters", [])][1:]
    expected = item["markers_ms"]
    check(metadata["cut_markers_ms"] == expected and len(markers) == len(expected), "Incorrect number or position of source markers")
    check(all(abs(a - b) <= 50 for a, b in zip(markers, expected)), "Embedded marker timestamp mismatch")
    subtitle = folder / (stem + ".srt")
    check(metadata["subtitle"] == subtitle.name, "Subtitle does not belong to this video")
    count = segment_count(subtitle, duration)
    evidence = metadata.get("subtitle_evidence", {})
    with subtitle.open("rb") as stream:
        subtitle_hash = hashlib.file_digest(stream, "sha256").hexdigest()
    if "subtitle_sha256" in evidence:
        check(evidence["subtitle_sha256"] == subtitle_hash, "Subtitle hash differs from metadata")
    invalid = len(evidence.get("invalid_utf8", []))
    boundary_edits = len(evidence.get("boundary_edits", []))
    if invalid or boundary_edits:
        check(evidence.get("text_quality") == "NEEDS_REVIEW" and evidence.get("native_ref"), "Missing review evidence for normalized bytes")
    check(metadata["transcript_status"] == ("SEGMENTS" if count else "NO_SEGMENTS"), "Subtitle status mismatch")
    return dict(video=f"{folder.name}/{path.name}", duration_s=duration, bitrate_bps=bitrate,
                markers=len(markers), subtitle_segments=count, invalid_utf8_spans=invalid, time_adjustments=boundary_edits, subtitle_sha256=subtitle_hash, sha256=checksum)


def verify(plan, probe):
    for ancestor in (OUTPUT, *OUTPUT.parents):
        check(not ancestor.is_symlink() and not ancestor.is_junction(), "Destination crosses a linked directory")
    expected_events = plan["events"]
    actual = entries(OUTPUT)
    check(set(actual) == set(expected_events), "Unexpected or missing event folder")
    verified = []
    for name, items in expected_events.items():
        folder = actual[name]
        check(folder.is_dir(), f"Event is not a directory: {name}")
        found = entries(folder)
        expected_files = {"event.json"}
        for item in items:
            expected_files.update(item["stem"] + ext for ext in (".mp4", ".srt", ".json"))
        check(set(found) == expected_files and all(p.is_file() for p in found.values()), f"Incorrect files or nested folder: {name}")
        event = read_json(folder / "event.json")
        check(event["run_id"] == plan["run_id"] and event["event_source"] == name, "Incorrect event identity")
        check(sorted(event["outputs"]) == sorted(item["stem"] + ".mp4" for item in items), "Event manifest differs from plan")
        for item in items:
            verified.append(verify_video(folder, item, plan["run_id"], plan["input_revision"], probe))
    return dict(run_id=plan["run_id"], max_folder_depth=1, events=len(actual), videos=verified)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("plan", type=Path, help="Expected outputs from classification, before publication")
    parser.add_argument("--ffprobe", type=Path, required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(verify(read_json(args.plan), args.ffprobe), ensure_ascii=False))
    except (KeyError, ValueError, OSError, StopIteration, subprocess.SubprocessError) as exc:
        print(json.dumps({"status": "FAILED", "cause": str(exc)}, ensure_ascii=False))
        sys.exit(1)
