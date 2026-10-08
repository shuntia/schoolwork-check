/**
 * Google Apps Script: dump Google Classroom coursework as homework rows.
 *
 * Setup (once, at script.google.com while signed in to the school account):
 *   1. New project, paste this file.
 *   2. Services (+) -> add "Google Classroom API".
 *   3. Run dumpHomework() once and accept the permission prompt.
 *   4. Triggers -> add time-driven trigger for dumpHomework, e.g. every hour.
 *
 * Output: a file "homework_classroom.sql" in Drive containing INSERT statements
 * in the same shape as schema.sql. Pull it down with `gdrive`/rclone or just
 * read it from Drive, then run:  sqlite3 homework.db < homework_classroom.sql
 */

var OUT_FILE = 'homework_classroom.sql';

function dumpHomework() {
  var rows = collectRows();
  var sql = ['BEGIN;'].concat(rows.map(asInsert)).concat(['COMMIT;']).join('\n') + '\n';
  writeDriveFile(OUT_FILE, sql);
  Logger.log(rows.length + ' rows written to ' + OUT_FILE);
}

function collectRows() {
  var fetched = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  var rows = [];
  var courses = listAll(function (tok) {
    return Classroom.Courses.list({ studentId: 'me', courseStates: ['ACTIVE'], pageToken: tok });
  }, 'courses');

  courses.forEach(function (course) {
    var work = listAll(function (tok) {
      return Classroom.Courses.CourseWork.list(course.id, { orderBy: 'dueDate asc', pageToken: tok });
    }, 'courseWork');

    // One call per course for all my submissions, keyed by courseWorkId.
    var subs = {};
    listAll(function (tok) {
      return Classroom.Courses.CourseWork.StudentSubmissions.list(course.id, '-', { userId: 'me', pageToken: tok });
    }, 'studentSubmissions').forEach(function (s) { subs[s.courseWorkId] = s; });

    work.forEach(function (w) {
      rows.push({
        source: 'classroom',
        source_id: course.id + ':' + w.id,
        course: course.name,
        title: w.title || '(untitled)',
        kind: kindOf(w.workType),
        due_at: dueIso(w),
        status: statusOf(subs[w.id], w),
        points: w.maxPoints == null ? null : w.maxPoints,
        url: w.alternateLink || null,
        fetched_at: fetched
      });
    });
  });
  return rows;
}

function kindOf(t) {
  return { ASSIGNMENT: 'assignment', SHORT_ANSWER_QUESTION: 'question', MULTIPLE_CHOICE_QUESTION: 'question' }[t] || 'assignment';
}

function dueIso(w) {
  if (!w.dueDate) return null;
  var d = w.dueDate, t = w.dueTime || {};
  return new Date(Date.UTC(d.year, d.month - 1, d.day, t.hours || 0, t.minutes || 0)).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

function statusOf(sub, w) {
  if (!sub) return 'todo';
  if (sub.state === 'RETURNED') return sub.assignedGrade != null ? 'graded' : 'submitted';
  if (sub.state === 'TURNED_IN') return sub.late ? 'late' : 'submitted';
  var due = dueIso(w);
  if (due && new Date(due) < new Date()) return 'missing';
  return 'todo';
}

function listAll(fetchPage, key) {
  var out = [], tok = null;
  do {
    var page = fetchPage(tok) || {};
    out = out.concat(page[key] || []);
    tok = page.nextPageToken;
  } while (tok);
  return out;
}

var COLUMNS = ['source', 'source_id', 'course', 'title', 'kind', 'due_at', 'status', 'points', 'url', 'fetched_at'];

function lit(v) {
  if (v === null || v === undefined) return 'NULL';
  if (typeof v === 'number') return String(v);
  return "'" + String(v).replace(/'/g, "''") + "'";
}

function asInsert(row) {
  var updates = COLUMNS.filter(function (c) { return c !== 'source' && c !== 'source_id'; })
    .map(function (c) { return c + '=excluded.' + c; }).join(', ');
  return 'INSERT INTO homework (' + COLUMNS.join(', ') + ') VALUES (' +
    COLUMNS.map(function (c) { return lit(row[c]); }).join(', ') + ')\n' +
    '  ON CONFLICT(source, source_id) DO UPDATE SET ' + updates + ';';
}

function writeDriveFile(name, content) {
  var files = DriveApp.getFilesByName(name);
  if (files.hasNext()) files.next().setContent(content);
  else DriveApp.createFile(name, content, MimeType.PLAIN_TEXT);
}
