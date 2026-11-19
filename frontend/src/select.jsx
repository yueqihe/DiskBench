import { useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown } from "lucide-react";

// Keep focus on the trigger while navigating the list, including in dialogs.
export function Select({ value, options, onValueChange, disabled = false, placeholder = "请选择", className = "", ...props }) {
  const id = useId();
  const triggerRef = useRef(null);
  const menuRef = useRef(null);
  const placeRef = useRef(null);
  const searchRef = useRef({ text: "", time: 0 });
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const items = options.flatMap(option => option.options || [option]);
  const selected = items.findIndex(option => String(option.value) === String(value));
  const enabled = items.map((option, index) => option.disabled ? -1 : index).filter(index => index >= 0);
  const usableActive = enabled.includes(active) ? active : enabled.includes(selected) ? selected : enabled[0];

  function show(edge) {
    if (triggerRef.current.matches(":disabled") || !enabled.length) return;
    searchRef.current = { text: "", time: 0 };
    setActive(edge === "first" ? enabled[0] : edge === "last" ? enabled.at(-1) : enabled.includes(selected) ? selected : enabled[0]);
    setOpen(true);
  }

  function choose(index) {
    if (triggerRef.current.matches(":disabled") || items[index]?.disabled || !items[index]) return;
    setOpen(false);
    triggerRef.current.focus({ preventScroll: true });
    onValueChange(String(items[index].value));
  }

  useLayoutEffect(() => {
    if (!open) return;
    const trigger = triggerRef.current;
    const menu = menuRef.current;
    menu.showPopover?.();
    const place = () => {
      const rect = trigger.getBoundingClientRect();
      const viewport = window.visualViewport;
      const left = (viewport?.offsetLeft || 0) + 8;
      const top = (viewport?.offsetTop || 0) + 8;
      const right = left + (viewport?.width || window.innerWidth) - 16;
      const bottom = top + (viewport?.height || window.innerHeight) - 16;
      if (!trigger.isConnected || trigger.matches(":disabled") || rect.bottom <= top - 8 || rect.top >= bottom + 8) {
        setOpen(false);
        return;
      }
      menu.style.width = `${Math.max(0, Math.min(Math.max(rect.width, 160), right - left))}px`;
      // Fit the option list to the application viewport instead of a fixed cap.
      menu.style.maxHeight = `${Math.max(0, bottom - top)}px`;
      const wanted = menu.getBoundingClientRect().height;
      const below = Math.max(0, bottom - rect.bottom - 6);
      const above = Math.max(0, rect.top - top - 6);
      const upwards = below < wanted && above > below;
      menu.style.maxHeight = `${upwards ? above : below}px`;
      const menuRect = menu.getBoundingClientRect();
      menu.style.left = `${Math.max(left, Math.min(rect.left, right - menuRect.width))}px`;
      menu.style.top = `${Math.max(top, Math.min(upwards ? rect.top - menuRect.height - 6 : rect.bottom + 6, bottom - menuRect.height))}px`;
    };
    const outside = event => {
      if (!trigger.contains(event.target) && !menu.contains(event.target)) setOpen(false);
    };
    const scroll = event => { if (!menu.contains(event.target)) place(); };
    const close = () => setOpen(false);
    placeRef.current = place;
    place();
    const observer = new ResizeObserver(place);
    observer.observe(trigger);
    document.addEventListener("pointerdown", outside, true);
    window.addEventListener("resize", place);
    window.addEventListener("scroll", scroll, true);
    window.addEventListener("blur", close);
    window.visualViewport?.addEventListener("resize", place);
    window.visualViewport?.addEventListener("scroll", place);
    return () => {
      placeRef.current = null;
      observer.disconnect();
      document.removeEventListener("pointerdown", outside, true);
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", scroll, true);
      window.removeEventListener("blur", close);
      window.visualViewport?.removeEventListener("resize", place);
      window.visualViewport?.removeEventListener("scroll", place);
    };
  }, [open]);

  useLayoutEffect(() => {
    if (open) placeRef.current?.();
    if (open && triggerRef.current.matches(":disabled")) setOpen(false);
    if (open && usableActive !== undefined) {
      const menu = menuRef.current;
      const option = menu.querySelector(`[data-option-index="${usableActive}"]`);
      if (option && option.offsetTop < menu.scrollTop) menu.scrollTop = option.offsetTop;
      else if (option && option.offsetTop + option.offsetHeight > menu.scrollTop + menu.clientHeight) {
        menu.scrollTop = option.offsetTop + option.offsetHeight - menu.clientHeight;
      }
    }
  });

  function keyDown(event) {
    if (event.key === "Tab") { setOpen(false); return; }
    if (event.key === "Escape" && open) {
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      return;
    }
    if (["ArrowDown", "ArrowUp", "Home", "End", "Enter", " "].includes(event.key)) {
      event.preventDefault();
      if (!open) show(event.key === "Home" ? "first" : event.key === "End" ? "last" : undefined);
      else if (event.key === "Enter" || event.key === " ") choose(usableActive);
      else if (event.key === "Home" || event.key === "End") setActive(event.key === "Home" ? enabled[0] : enabled.at(-1));
      else {
        const next = enabled.indexOf(usableActive) + (event.key === "ArrowDown" ? 1 : -1);
        setActive(enabled[Math.max(0, Math.min(enabled.length - 1, next))]);
      }
    } else if (event.key.length === 1 && !event.ctrlKey && !event.altKey && !event.metaKey) {
      event.preventDefault();
      const now = Date.now();
      const text = `${now - searchRef.current.time < 700 ? searchRef.current.text : ""}${event.key.toLocaleLowerCase()}`;
      searchRef.current = { text, time: now };
      const prefix = [...text].every(char => char === text[0]) ? text[0] : text;
      const start = enabled.indexOf(open ? usableActive : selected);
      const ordered = [...enabled.slice(start + 1), ...enabled.slice(0, start + 1)];
      const match = ordered.find(index => String(items[index].label).toLocaleLowerCase().startsWith(prefix));
      if (match !== undefined) { setActive(match); setOpen(true); }
    }
  }

  let index = 0;
  function renderOption(option) {
    const optionIndex = index++;
    return <div key={String(option.value)} id={`${id}-option-${optionIndex}`} role="option"
      aria-selected={selected === optionIndex} aria-disabled={option.disabled || undefined}
      data-option-index={optionIndex} className={`select-option${usableActive === optionIndex ? " active" : ""}`}
      onPointerMove={() => { if (!option.disabled && active !== optionIndex) setActive(optionIndex); }}
      onMouseDown={event => event.preventDefault()} onClick={event => { event.stopPropagation(); choose(optionIndex); }}>
      <span>{option.label}</span><span className="select-option-check" aria-hidden="true">{selected === optionIndex && <Check size={14} />}</span>
    </div>;
  }

  return <>
    <button {...props} ref={triggerRef} type="button" role="combobox" className={`select-trigger ${className}`.trim()}
      disabled={disabled} aria-haspopup="listbox" aria-expanded={open} aria-controls={open ? `${id}-list` : undefined}
      aria-activedescendant={open && usableActive !== undefined ? `${id}-option-${usableActive}` : undefined}
      data-tooltip={open ? undefined : props["data-tooltip"]}
      onClick={() => open ? setOpen(false) : show()} onKeyDown={keyDown}
      onBlur={event => { if (!menuRef.current?.contains(event.relatedTarget)) setOpen(false); }}>
      <span className="select-value">{items[selected]?.label || placeholder}</span><ChevronDown className="select-chevron" size={14} aria-hidden="true" />
    </button>
    {open && createPortal(<div ref={menuRef} id={`${id}-list`} className="select-menu" role="listbox"
      aria-label={props["aria-label"]} popover="manual">
      {options.map(option => option.options ? <div key={option.label} role="group" aria-label={option.label}>
        <div className="select-group-label" aria-hidden="true">{option.label}</div>{option.options.map(renderOption)}
      </div> : renderOption(option))}
    </div>, triggerRef.current?.closest("dialog[open]") || document.body)}
  </>;
}
