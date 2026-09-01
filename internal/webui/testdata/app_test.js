"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class FakeEventTarget {
  constructor() {
    this.listeners = new Map();
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  dispatch(type, properties = {}) {
    let prevented = false;
    const event = {
      target: this,
      preventDefault() { prevented = true; },
      ...properties,
    };
    (this.listeners.get(type) || []).forEach((listener) => listener(event));
    event.prevented = prevented;
    return event;
  }
}

class FakeElement extends FakeEventTarget {
  constructor(properties = {}) {
    super();
    Object.assign(this, {
      checked: false,
      disabled: false,
      hidden: false,
      value: "",
      dataset: {},
      children: [],
      ...properties,
    });
    this.selectors = new Map();
  }

  querySelector(selector) {
    return this.selectors.get(selector) || null;
  }

  querySelectorAll(selector) {
    return this.selectors.get(selector) || [];
  }

  replaceChildren(...children) {
    this.children = children;
  }

  appendChild(child) {
    this.children.push(child);
    return child;
  }

  prepend(child) {
    this.children.unshift(child);
    return child;
  }

  setAttribute() {}
}

function mapSelector(element, selector, value) {
  element.selectors.set(selector, value);
}

const workbench = new FakeElement();
const batchForm = new FakeElement({ action: "/api/jobs/batch" });
const actionSelect = new FakeElement();
const verdictLabel = new FakeElement();
const verdictSelect = new FakeElement({ value: "suitable" });
const submitButton = new FakeElement();
const selectAll = new FakeElement();
const selectionText = new FakeElement();
const resultPanel = new FakeElement();
const pageInput = new FakeElement({ value: "3" });
const filterInput = new FakeElement();
const boxes = [
  new FakeElement({ dataset: { jobId: "1", assessmentAllowed: "true", reviewAllowed: "true", outreachAllowed: "false", jdHash: "hash-1" } }),
  new FakeElement({ dataset: { jobId: "2", assessmentAllowed: "false", reviewAllowed: "true", outreachAllowed: "true", jdHash: "hash-2" } }),
];
mapSelector(workbench, "#job-batch-form", batchForm);
mapSelector(workbench, "#job-batch-action", actionSelect);
mapSelector(workbench, "#job-batch-verdict-label", verdictLabel);
mapSelector(workbench, "#job-batch-verdict", verdictSelect);
mapSelector(workbench, "#job-batch-submit", submitButton);
mapSelector(workbench, "#job-select-all", selectAll);
mapSelector(workbench, "#job-batch-selection", selectionText);
mapSelector(workbench, "#job-batch-result", resultPanel);
mapSelector(workbench, ".job-select", boxes);
mapSelector(workbench, ".quick-action[data-quick-action]", []);
mapSelector(workbench, ".job-filters input, .job-filters select", [filterInput]);
mapSelector(workbench, ".job-filters input[name='page']", pageInput);

const policyRoot = new FakeElement();
const policySample = new FakeElement({ value: "10", checked: true });
const policyValidation = new FakeElement({ checked: false });
mapSelector(policyRoot, ".policy-sample-checkbox", [policySample]);
mapSelector(policyRoot, "#policy-validation-enabled", policyValidation);

const windowObject = new FakeEventTarget();
windowObject.alert = () => {};
windowObject.confirm = () => true;
const documentObject = {
  querySelector(selector) {
    if (selector === "#job-workbench") return workbench;
    if (selector === "#policy-optimization") return policyRoot;
    return null;
  },
  createElement() { return new FakeElement(); },
};

const context = {
  Array, Boolean, Date, JSON, Math, Number, Promise, String,
  console, document: documentObject, fetch: async () => { throw new Error("unexpected fetch"); },
  window: windowObject,
};
vm.runInNewContext(fs.readFileSync(path.join(__dirname, "..", "assets", "app.js"), "utf8"), context);

assert.equal(boxes[0].disabled, true, "batch boxes start disabled until an action is selected");
assert.equal(selectAll.disabled, true, "select all starts disabled");
actionSelect.value = "assessment";
actionSelect.dispatch("change");
assert.equal(boxes[0].disabled, false, "assessment enables executable rows");
assert.equal(boxes[1].disabled, true, "assessment disables unavailable rows");
selectAll.checked = true;
selectAll.dispatch("change");
assert.equal(boxes[0].checked, true, "select all selects executable current-page rows");
assert.equal(boxes[1].checked, false, "select all leaves unavailable rows unselected");
assert.equal(submitButton.disabled, false, "selected executable row enables submit");
filterInput.dispatch("change");
assert.equal(pageInput.value, "1", "filter changes reset the page");
assert.equal(boxes[0].checked, false, "filter changes clear current selection");
assert.equal(submitButton.disabled, true, "cleared selection disables submit");

const beforeUnload = windowObject.listeners.get("beforeunload")[0];
let event = beforeUnload({ preventDefault() {}, returnValue: "" });
assert.equal(event, undefined, "unchanged policy samples do not warn on leave");
policySample.checked = false;
policySample.dispatch("change");
const leaveEvent = { prevented: false, preventDefault() { this.prevented = true; } };
beforeUnload(leaveEvent);
assert.equal(leaveEvent.prevented, true, "changed policy sample selection warns before leaving");
assert.match(leaveEvent.returnValue, /样本选择/);

console.log("app.js DOM behavior tests passed");
