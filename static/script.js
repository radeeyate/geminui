let deleteChat = async (chatID) => {
    fetch(`/api/delete/${chatID}`, { method: "DELETE" }).then(response => {
        if (response.ok) {
            const chatItem = document.querySelector(`.chat-item a[href="/chat/${chatID}"]`).closest("li");
            chatItem.remove();

            if (window.location.pathname.includes(`/chat/${chatID}`)) {
                window.location.href = "/";
            }
        } else {
            console.error("Failed to delete chat");
        }
    }).catch(error => {
        console.error("Error deleting chat: ", error)
    })
}