const tabs = document.querySelectorAll(".tab");
const form = document.getElementById("add-form");
const input = document.getElementById("item-text");
const list = document.getElementById("item-list");

let currentList = "todo";

tabs.forEach((tab) => {
  tab.addEventListener("click", () => {
    tabs.forEach((t) => t.classList.remove("active"));
    tab.classList.add("active");
    currentList = tab.dataset.list;
    loadItems();
  });
});

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text) return;

  await fetch("/api/items", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ list_type: currentList, text }),
  });

  input.value = "";
  loadItems();
});

async function loadItems() {
  const res = await fetch(`/api/items?list=${currentList}`);
  const items = await res.json();

  list.innerHTML = "";
  items.forEach((item) => {
    const li = document.createElement("li");
    if (item.done) li.classList.add("done");

    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = item.done;
    checkbox.addEventListener("change", () => toggleItem(item.id));

    const span = document.createElement("span");
    span.className = "item-text";
    span.textContent = item.text;

    const delBtn = document.createElement("button");
    delBtn.className = "delete-btn";
    delBtn.textContent = "✕";
    delBtn.addEventListener("click", () => deleteItem(item.id));

    li.appendChild(checkbox);
    li.appendChild(span);
    li.appendChild(delBtn);
    list.appendChild(li);
  });
}

async function toggleItem(id) {
  await fetch(`/api/items/${id}/toggle`, { method: "POST" });
  loadItems();
}

async function deleteItem(id) {
  await fetch(`/api/items/${id}`, { method: "DELETE" });
  loadItems();
}

loadItems();
